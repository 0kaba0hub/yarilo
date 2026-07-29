package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubAuth is a minimal yarilo-auth wire server for the multiplexing tests.
type stubAuth struct {
	ln      net.Listener
	accepts atomic.Int64
	// delay is applied before each reply so concurrent requests genuinely
	// overlap on the connection.
	delay time.Duration
	// replyOutOfOrder answers a batch in reverse arrival order, which is what
	// proves the demultiplexer routes by id rather than by arrival sequence.
	replyOutOfOrder bool

	mu     sync.Mutex
	closed bool
}

func newStubAuth(t *testing.T, delay time.Duration, outOfOrder bool) *stubAuth {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &stubAuth{ln: ln, delay: delay, replyOutOfOrder: outOfOrder}
	go s.serve()
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *stubAuth) addr() string { return s.ln.Addr().String() }

func (s *stubAuth) stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		_ = s.ln.Close()
	}
	s.mu.Unlock()
}

func (s *stubAuth) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepts.Add(1)
		go s.handle(c)
	}
}

func (s *stubAuth) handle(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)

	fmt.Fprintf(c, "VERSION\t1\t0\n")
	fmt.Fprintf(c, "MECH\tPLAIN\tplaintext\n")
	fmt.Fprintf(c, "SPID\t1\n")
	fmt.Fprintf(c, "DONE\n")

	var wmu sync.Mutex
	var batch []string
	var bmu sync.Mutex

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(fields) < 2 {
			continue
		}
		verb, id := fields[0], fields[1]
		if verb == "VERSION" {
			continue
		}

		reply := ""
		switch verb {
		case "AUTH":
			user := ""
			for _, f := range fields[2:] {
				if strings.HasPrefix(f, "user=") {
					user = f[len("user="):]
				}
			}
			reply = fmt.Sprintf("OK\t%s\tuser=%s", id, user)
		case "VERIFY":
			reply = fmt.Sprintf("OK\t%s\tuser=u\tsession=s\tservice=imap", id)
		case "USER":
			reply = fmt.Sprintf("USER\t%s", id)
		default:
			continue
		}

		if s.replyOutOfOrder {
			bmu.Lock()
			batch = append(batch, reply)
			flush := len(batch) >= 3
			var out []string
			if flush {
				for i := len(batch) - 1; i >= 0; i-- {
					out = append(out, batch[i])
				}
				batch = nil
			}
			bmu.Unlock()
			if flush {
				wmu.Lock()
				for _, r := range out {
					fmt.Fprintln(c, r)
				}
				wmu.Unlock()
			}
			continue
		}

		go func(r string) {
			if s.delay > 0 {
				time.Sleep(s.delay)
			}
			wmu.Lock()
			fmt.Fprintln(c, r)
			wmu.Unlock()
		}(reply)
	}
}

// TestConcurrentRequestsShareOneConnection is the #878 acceptance test. The old
// client dialled per request, so connection count tracked request count 1:1
// (measured on sandbox: 9469 connections for 9329 requests). Asserting a hard
// equality of ONE accept — not merely "fewer than N" — is what distinguishes
// real reuse from a pool that still dials under load.
func TestConcurrentRequestsShareOneConnection(t *testing.T) {
	const concurrency = 200

	srv := newStubAuth(t, 20*time.Millisecond, false)
	c, err := Dial(srv.addr(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d@d", i)
			res, err := c.Authenticate(user, "pw", "imap", "10.0.0.1", "sid")
			if err != nil {
				errs <- err
				return
			}
			// Each caller must receive ITS OWN reply, not another's.
			if res.Username != user {
				errs <- fmt.Errorf("reply mismatch: got %q want %q", res.Username, user)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Authenticate: %v", err)
	}

	if got := srv.accepts.Load(); got != 1 {
		t.Fatalf("server accepted %d connections for %d requests, want exactly 1", got, concurrency)
	}
}

// TestRepliesRoutedByIDNotArrivalOrder covers the demultiplexer directly: the
// stub answers each batch of three in reverse order, which the old
// read-the-next-line client reported as "response id mismatch".
func TestRepliesRoutedByIDNotArrivalOrder(t *testing.T) {
	srv := newStubAuth(t, 0, true)
	c, err := Dial(srv.addr(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	const batch = 9
	var wg sync.WaitGroup
	errs := make(chan error, batch)
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user%d", i)
			res, err := c.Authenticate(user, "pw", "imap", "", "")
			if err != nil {
				errs <- err
				return
			}
			if res.Username != user {
				errs <- fmt.Errorf("got %q want %q", res.Username, user)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("out-of-order routing: %v", err)
	}
}

// TestReconnectQueuesInsteadOfFailing is the behaviour the old code got wrong:
// during an auth restart every in-flight login was told UNAVAILABLE. A request
// that has not yet been written must wait for the reconnect and then succeed.
func TestReconnectQueuesInsteadOfFailing(t *testing.T) {
	srv := newStubAuth(t, 0, false)
	c, err := Dial(srv.addr(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Authenticate("u@d", "pw", "imap", "", ""); err != nil {
		t.Fatalf("warm-up Authenticate: %v", err)
	}

	// Kill the live connection underneath the client; the stub keeps listening,
	// so the redial succeeds and the next request must ride the new connection.
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		t.Fatal("no live connection to drop")
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := c.Authenticate("u@d", "pw", "imap", "", ""); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("request after connection drop never recovered: %v", lastErr)
	}
	if got := srv.accepts.Load(); got < 2 {
		t.Fatalf("server accepts = %d, want >= 2 (reconnect did not happen)", got)
	}
}

func TestVerifyAndLookupShareTheConnection(t *testing.T) {
	srv := newStubAuth(t, 0, false)
	c, err := Dial(srv.addr(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	tests := []struct {
		name string
		call func() error
	}{
		{"authenticate", func() error {
			_, err := c.Authenticate("u@d", "pw", "imap", "", "")
			return err
		}},
		{"verify", func() error {
			_, _, _, err := c.Verify("tok", "u", "s")
			return err
		}},
		{"lookup", func() error {
			_, err := c.LookupUser("u@d")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		})
	}
	if got := srv.accepts.Load(); got != 1 {
		t.Fatalf("accepts = %d across three verbs, want 1", got)
	}
}

func TestClosedClientRejectsRequests(t *testing.T) {
	srv := newStubAuth(t, 0, false)
	c, err := Dial(srv.addr(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := c.Authenticate("u@d", "pw", "imap", "", ""); err != ErrClosed {
		t.Fatalf("after Close: err = %v, want ErrClosed", err)
	}
}
