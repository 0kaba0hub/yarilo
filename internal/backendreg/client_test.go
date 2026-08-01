package backendreg

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockDirector is a minimal director-side listener: it plays the server
// handshake (VERSION / HOST-HAND-* / DONE) and records every line the client
// sends after its own handshake.
type mockDirector struct {
	ln net.Listener

	sendPing bool // when set, the mock sends one PING right after the handshake

	mu      sync.Mutex
	lines   []string
	conns   []net.Conn
	connsWG sync.WaitGroup
}

func newMockDirector(t *testing.T) *mockDirector {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &mockDirector{ln: ln}
	go m.acceptLoop()
	return m
}

func (m *mockDirector) addr() string { return m.ln.Addr().String() }

func (m *mockDirector) acceptLoop() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.connsWG.Add(1)
		go m.handle(conn)
	}
}

func (m *mockDirector) handle(conn net.Conn) {
	defer m.connsWG.Done()
	defer conn.Close()
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	// Server handshake.
	wr := bufio.NewWriter(conn)
	wr.WriteString("VERSION\tyarilo-director\t1\t0\n") //nolint:errcheck
	wr.WriteString("HOST-HAND-START\n")                //nolint:errcheck
	wr.WriteString("HOST-HAND-END\n")                  //nolint:errcheck
	wr.WriteString("DONE\n")                           //nolint:errcheck
	if m.sendPing {
		wr.WriteString("PING\n") //nolint:errcheck
	}
	wr.Flush() //nolint:errcheck

	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			m.mu.Lock()
			m.lines = append(m.lines, strings.TrimRight(line, "\n"))
			m.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (m *mockDirector) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.lines))
	copy(out, m.lines)
	return out
}

func (m *mockDirector) close() { m.ln.Close() }

// dropConns force-closes every accepted connection so the client sees a read
// error and redials the same listener.
func (m *mockDirector) dropConns() {
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// reset clears recorded lines so a post-reconnect assertion is unambiguous.
func (m *mockDirector) reset() {
	m.mu.Lock()
	m.lines = nil
	m.mu.Unlock()
}

// waitFor polls the recorded lines until pred is satisfied or timeout.
func (m *mockDirector) waitFor(t *testing.T, pred func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := m.snapshot()
		if pred(got) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := m.snapshot()
	if pred(got) {
		return got
	}
	t.Fatalf("timeout waiting for condition; got lines: %v", got)
	return nil
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestClient_HandshakeAndHeartbeat(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.7",
		Port:         10143,
		Tag:          "imap-a",
		Vhosts:       100,
		Interval:     50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// ME handshake then at least two heartbeats with rising seq
	lines := md.waitFor(t, func(ls []string) bool {
		return countPrefix(ls, "ME\t") >= 1 && countPrefix(ls, "BACKEND-UP\t") >= 2
	})

	if got := countPrefix(lines, "ME\t"); got != 1 {
		t.Fatalf("want exactly one ME handshake, got %d in %v", got, lines)
	}
	var ups []string
	for _, l := range lines {
		if strings.HasPrefix(l, "BACKEND-UP\t") {
			ups = append(ups, l)
		}
	}
	// fields: BACKEND-UP\tip\tport\ttag\tvhosts\tseq
	first := strings.Split(ups[0], "\t")
	if len(first) != 6 {
		t.Fatalf("BACKEND-UP field count = %d, want 6: %q", len(first), ups[0])
	}
	if first[1] != "10.0.0.7" || first[2] != "10143" || first[3] != "imap-a" || first[4] != "100" {
		t.Fatalf("BACKEND-UP identity fields wrong: %q", ups[0])
	}
	// seq is time-seeded, so assert monotonic increase, not a fixed 1/2
	firstSeq, err := strconv.ParseUint(first[5], 10, 64)
	if err != nil {
		t.Fatalf("first heartbeat seq not a uint: %q", first[5])
	}
	second := strings.Split(ups[1], "\t")
	secondSeq, err := strconv.ParseUint(second[5], 10, 64)
	if err != nil {
		t.Fatalf("second heartbeat seq not a uint: %q", second[5])
	}
	if secondSeq != firstSeq+1 {
		t.Fatalf("second seq = %d, want first+1 (%d)", secondSeq, firstSeq+1)
	}
}

func TestClient_UnhealthySkipsHeartbeat(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.8",
		Port:         10143,
		Interval:     30 * time.Millisecond,
		Healthy:      func() bool { return false },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// ME handshake still lands, but no BACKEND-UP is sent
	md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "ME\t") >= 1 })
	time.Sleep(150 * time.Millisecond)
	if n := countPrefix(md.snapshot(), "BACKEND-UP\t"); n != 0 {
		t.Fatalf("unhealthy backend sent %d heartbeats, want 0", n)
	}
}

func TestClient_LeaveSendsBackendDown(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.9",
		Port:         10143,
		Interval:     50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "BACKEND-UP\t") >= 1 })
	c.Leave()
	lines := md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "BACKEND-DOWN\t") >= 1 })

	var down string
	for _, l := range lines {
		if strings.HasPrefix(l, "BACKEND-DOWN\t") {
			down = l
		}
	}
	if down != "BACKEND-DOWN\t10.0.0.9" {
		t.Fatalf("BACKEND-DOWN = %q, want BACKEND-DOWN\\t10.0.0.9", down)
	}
}

func TestClient_DrainSendsBackendFlush(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.10",
		Port:         10143,
		Interval:     50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "BACKEND-UP\t") >= 1 })
	c.Drain()
	lines := md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "BACKEND-FLUSH\t") >= 1 })

	var flush string
	for _, l := range lines {
		if strings.HasPrefix(l, "BACKEND-FLUSH\t") {
			flush = l
		}
	}
	if flush != "BACKEND-FLUSH\t10.0.0.10" {
		t.Fatalf("BACKEND-FLUSH = %q, want BACKEND-FLUSH\\t10.0.0.10", flush)
	}
}

func TestClient_ReconnectsAfterDrop(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.11",
		Port:         10143,
		Interval:     40 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "ME\t") >= 1 })
	// force-close the active connection; the client must redial,
	// re-handshake and resume heartbeating
	md.reset()
	md.dropConns()
	md.waitFor(t, func(ls []string) bool {
		return countPrefix(ls, "ME\t") >= 1 && countPrefix(ls, "BACKEND-UP\t") >= 1
	})
}

func TestClient_NoDirectorAddrIsNoop(t *testing.T) {
	c := New(Options{SelfIP: "10.0.0.12", Port: 10143})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with empty DirectorAddr should return immediately")
	}
}

// TestClient_RepliesPongToPing: the director's PING keepalive must be
// answered with PONG or the director closes the registration.
func TestClient_RepliesPongToPing(t *testing.T) {
	md := newMockDirector(t)
	defer md.close()
	md.sendPing = true // mock sends one PING right after the handshake

	c := New(Options{
		DirectorAddr: md.addr(),
		SelfIP:       "10.0.0.20",
		Port:         10143,
		Interval:     50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	md.waitFor(t, func(ls []string) bool { return countPrefix(ls, "PONG") >= 1 })
}
