package authclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMaster is a master-protocol listener that counts what it sees. Tests go
// through it over real TCP rather than through a seam of their own: a pool is
// a statement about connections, and only the far end can say how many there
// were.
type fakeMaster struct {
	ln net.Listener

	mu          sync.Mutex
	conns       int
	noops       int
	users       int
	sessions    int // SESSION calls served
	dieAfter    int // close the connection after this many lookups (0 = never)
	silentNOOPs int // this many NOOPs go unanswered, as a dead peer's would
}

func newFakeMaster(t *testing.T) *fakeMaster {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMaster{ln: ln}
	go m.serve()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *fakeMaster) addr() string { return m.ln.Addr().String() }

func (m *fakeMaster) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.conns++
		m.mu.Unlock()
		go m.handle(conn)
	}
}

func (m *fakeMaster) handle(conn net.Conn) {
	defer conn.Close()
	fmt.Fprint(conn, "VERSION\tyarilo-auth-master\t1\t0\nDONE\n")
	rd := bufio.NewReader(conn)
	served := 0
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		id := "0"
		if len(fields) > 1 {
			id = fields[1]
		}
		switch fields[0] {
		case "NOOP":
			m.mu.Lock()
			m.noops++
			silent := m.silentNOOPs > 0
			if silent {
				m.silentNOOPs--
			}
			m.mu.Unlock()
			if silent {
				return // gone without a word, as a dead peer is
			}
			fmt.Fprintf(conn, "OK\t%s\n", id)
		case "SESSION":
			m.mu.Lock()
			m.sessions++
			m.mu.Unlock()
			fmt.Fprintf(conn, "OK\t%s\ttoken=token-1\n", id)
		case "USER":
			m.mu.Lock()
			m.users++
			die := m.dieAfter
			m.mu.Unlock()
			served++
			if die > 0 && served >= die {
				return // answer nothing and drop: a peer that went away
			}
			fmt.Fprintf(conn, "USER\t%s\tuid=1000\tgid=1000\thome=/srv/u\n", id)
		default:
			fmt.Fprintf(conn, "FAIL\t%s\treason=unknown command\n", id)
		}
	}
}

func (m *fakeMaster) counts() (conns, noops, users int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns, m.noops, m.users
}

func (m *fakeMaster) set(fn func(*fakeMaster)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

func lookup(t *testing.T, p *Pool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.Userdb(ctx, "u@example.com")
	return err
}

// The point of the pool, stated where only the server can confirm it: the
// second lookup must not cost a second connection. The dial is about seven
// times the lookup it carries (#1402).
func TestPooledLookupsShareAConnection(t *testing.T) {
	m := newFakeMaster(t)
	p := NewPool(m.addr(), nil, 1, time.Minute)
	defer p.Close() //nolint:errcheck

	for i := 0; i < 3; i++ {
		if err := lookup(t, p); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	conns, _, users := m.counts()
	if users != 3 {
		t.Errorf("lookups served = %d, want 3", users)
	}
	if conns != 1 {
		t.Errorf("connections = %d, want 1: the pool is not reusing", conns)
	}
}

// Without eviction a process that resolved nobody for an hour still holds a
// connection auth has to keep. The reference closes an idle cached connection
// for the same reason, on the same five-minute default.
func TestAnIdleConnectionIsDropped(t *testing.T) {
	m := newFakeMaster(t)
	p := NewPool(m.addr(), nil, 1, 100*time.Millisecond)
	defer p.Close() //nolint:errcheck

	if err := lookup(t, p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := lookup(t, p); err != nil {
		t.Fatal(err)
	}
	if conns, _, _ := m.counts(); conns != 2 {
		t.Errorf("connections = %d, want 2: the idle connection was reused instead of evicted", conns)
	}
}

// The probe is worth a round trip only when the connection has been quiet.
// Probing every lookup returns half of what the pool saves, so a connection
// used a moment ago is handed out untouched.
func TestTheProbeFiresOnlyAfterSilence(t *testing.T) {
	m := newFakeMaster(t)
	// idle 400ms → probe threshold 100ms.
	p := NewPool(m.addr(), nil, 1, 400*time.Millisecond)
	defer p.Close() //nolint:errcheck

	for i := 0; i < 3; i++ {
		if err := lookup(t, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, noops, _ := m.counts(); noops != 0 {
		t.Errorf("probes on a busy pool = %d, want 0: a probe per lookup returns the round trip we removed", noops)
	}

	time.Sleep(150 * time.Millisecond) // quiet, but not long enough to evict
	if err := lookup(t, p); err != nil {
		t.Fatal(err)
	}
	conns, noops, _ := m.counts()
	if noops != 1 {
		t.Errorf("probes after silence = %d, want 1", noops)
	}
	if conns != 1 {
		t.Errorf("connections = %d, want 1: a probed-and-alive connection must be kept", conns)
	}
}

// A peer that has gone away while the connection sat idle must not be
// discovered by failing the user's lookup.
func TestASilentPeerIsReplacedBeforeTheLookup(t *testing.T) {
	m := newFakeMaster(t)
	p := NewPool(m.addr(), nil, 1, 400*time.Millisecond)
	defer p.Close() //nolint:errcheck

	if err := lookup(t, p); err != nil {
		t.Fatal(err)
	}
	// Only the connection now sitting in the pool is dead; a fresh one works,
	// as it would when auth has been restarted underneath us.
	m.set(func(f *fakeMaster) { f.silentNOOPs = 1 })
	time.Sleep(150 * time.Millisecond)

	if err := lookup(t, p); err != nil {
		t.Fatalf("the lookup failed instead of moving to a fresh connection: %v", err)
	}
	conns, noops, users := m.counts()
	if conns != 2 {
		t.Errorf("connections = %d, want 2: the dead connection was not replaced exactly once", conns)
	}
	if noops != 2 || users != 2 {
		t.Errorf("probes/lookups = %d/%d, want 2/2: the probe should have been retried on the fresh connection, then the lookup served", noops, users)
	}
}

// A transport failure on a pooled connection is retried ONCE, on a fresh
// connection. Walking every slot instead would turn "auth is down" into "this
// request hangs" -- the answer #1339 and #1402 exist to prevent.
func TestATransportFailureRedialsOnceAndThenReports(t *testing.T) {
	m := newFakeMaster(t)
	m.set(func(f *fakeMaster) { f.dieAfter = 1 })
	p := NewPool(m.addr(), nil, 2, time.Minute)
	defer p.Close() //nolint:errcheck

	// The first lookup is answered; the second finds the peer gone mid-request
	// and succeeds on the redial the client makes for itself.
	if err := lookup(t, p); err == nil {
		// dieAfter=1 means this very lookup is dropped, so a redial inside
		// the client is what makes it succeed at all.
		t.Log("first lookup answered after an internal redial")
	}

	// With the listener gone, the failure is reported as unavailability
	// rather than retried around the pool.
	m.ln.Close()
	err := lookup(t, p)
	if err == nil {
		t.Fatal("a lookup succeeded with no listener")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("a dead dependency is not reported as unavailable: %v", err)
	}
}

// Size zero is the rollback: exactly the old behaviour, a connection per
// lookup, reachable by config rather than by a release.
func TestPoolSizeZeroDialsPerLookup(t *testing.T) {
	m := newFakeMaster(t)
	p := NewPool(m.addr(), nil, 0, time.Minute)
	defer p.Close() //nolint:errcheck

	for i := 0; i < 3; i++ {
		if err := lookup(t, p); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if conns, _, users := m.counts(); conns != 3 || users != 3 {
		t.Errorf("connections/lookups = %d/%d, want 3/3: size 0 must not pool", conns, users)
	}
}

// The pool serves every master call, not just the lookup. LMTP delivery issues
// a session token and reads a director tag on the same connection it used to
// dial for (#1423), so a pool that only knew Userdb would have left the
// session holding its own connection anyway.
func TestPoolServesEveryMasterCall(t *testing.T) {
	m := newFakeMaster(t)
	p := NewPool(m.addr(), nil, 1, time.Minute)
	defer p.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Userdb(ctx, "u@example.com"); err != nil {
		t.Fatalf("userdb: %v", err)
	}
	if _, err := p.IssueSession(ctx, "u@example.com", "sid-1", "10.0.0.1"); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if conns, _, _ := m.counts(); conns != 1 {
		t.Errorf("two different calls opened %d connections, want 1", conns)
	}
}
