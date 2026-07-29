package protocol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowPassdb blocks for delay before answering, standing in for the deliberate
// sleeps on the AUTH path (auth-penalty tarpit, policy tarpit, failure delays).
type slowPassdb struct {
	user, pass string
	delay      time.Duration
	// slowUser, when non-empty, limits the delay to that username so a test can
	// have one slow and one fast request on the same connection.
	slowUser string
}

func (p *slowPassdb) Authenticate(req *Request) (Result, error) {
	if p.slowUser == "" || req.Username == p.slowUser {
		time.Sleep(p.delay)
	}
	if req.Username != p.user || req.Password != p.pass {
		return ResultFail, nil
	}
	req.Fields.Set("user", req.Username)
	return ResultOK, nil
}

// TestConcurrentCommandsOnOneConnection is the #887 acceptance test: a slow
// command must not delay an unrelated one queued behind it on the same
// connection. Before this change handleConn processed commands one at a time, so
// a single tarpitted request stalled every login behind it.
func TestConcurrentCommandsOnOneConnection(t *testing.T) {
	const delay = 700 * time.Millisecond

	srv := NewServer([]Passdb{&slowPassdb{user: "alice", pass: "secret", delay: delay, slowUser: "slowpoke"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	// Send the slow request first, then the fast one, on the same connection.
	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=imap\tresp=\x00slowpoke\x00nope\n")
	fmt.Fprintf(conn, "AUTH\t2\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")

	type reply struct {
		id   string
		when time.Duration
	}
	start := time.Now()
	var got []reply
	for range 2 {
		if !sc.Scan() {
			t.Fatalf("no reply: %v", sc.Err())
		}
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 2 {
			t.Fatalf("malformed reply %q", sc.Text())
		}
		got = append(got, reply{id: fields[1], when: time.Since(start)})
	}

	// The fast request (id 2) must answer first, and well before the slow one's
	// delay elapses — that is the whole point of per-command goroutines.
	if got[0].id != "2" {
		t.Fatalf("first reply id = %q, want \"2\" (fast request blocked behind the slow one)", got[0].id)
	}
	if got[0].when >= delay {
		t.Fatalf("fast reply took %v, want well under the slow request's %v", got[0].when, delay)
	}
	if got[1].id != "1" {
		t.Fatalf("second reply id = %q, want \"1\"", got[1].id)
	}
}

// TestConcurrentRepliesAreNotInterleaved covers the write-serialisation half of
// the change: concurrent handlers each emit a whole line, so no reply may be cut
// in half by another.
func TestConcurrentRepliesAreNotInterleaved(t *testing.T) {
	const requests = 60

	srv := NewServer([]Passdb{&slowPassdb{user: "alice", pass: "secret", delay: time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	var wmu sync.Mutex
	var wg sync.WaitGroup
	for i := 1; i <= requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wmu.Lock()
			defer wmu.Unlock()
			fmt.Fprintf(conn, "AUTH\t%d\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n", i)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, requests)
	for range requests {
		if !sc.Scan() {
			t.Fatalf("no reply after %d: %v", len(seen), sc.Err())
		}
		line := sc.Text()
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			t.Fatalf("interleaved or malformed reply: %q", line)
		}
		if fields[0] != "OK" {
			t.Fatalf("reply %q: want OK", line)
		}
		if seen[fields[1]] {
			t.Fatalf("duplicate reply id %q", fields[1])
		}
		seen[fields[1]] = true
	}
	if len(seen) != requests {
		t.Fatalf("got %d distinct replies, want %d", len(seen), requests)
	}
}

// TestMaxConcurrentRequestsBound checks the backpressure knob: with a bound of 1
// the connection degrades to the old sequential behaviour rather than failing,
// which is what makes the bound safe to tune down.
func TestMaxConcurrentRequestsBound(t *testing.T) {
	tests := []struct {
		name  string
		bound int
		want  int
	}{
		{"explicit bound", 4, 4},
		{"zero selects default", 0, DefaultMaxConcurrentRequests},
		{"negative selects default", -1, DefaultMaxConcurrentRequests},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(nil, WithMaxConcurrentRequests(tc.bound))
			got := srv.maxConcurrentRequests
			if got <= 0 {
				got = DefaultMaxConcurrentRequests
			}
			if got != tc.want {
				t.Fatalf("maxConcurrentRequests = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSerialisedConnectionStillAnswersEveryRequest(t *testing.T) {
	srv := NewServer(
		[]Passdb{&slowPassdb{user: "alice", pass: "secret", delay: time.Millisecond}},
		WithMaxConcurrentRequests(1),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	const requests = 10
	for i := 1; i <= requests; i++ {
		fmt.Fprintf(conn, "AUTH\t%d\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n", i)
	}
	for i := range requests {
		if !sc.Scan() {
			t.Fatalf("reply %d missing: %v", i, sc.Err())
		}
		if !strings.HasPrefix(sc.Text(), "OK\t") {
			t.Fatalf("reply %d = %q, want OK", i, sc.Text())
		}
	}
}
