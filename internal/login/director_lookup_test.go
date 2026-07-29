package login

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
)

// TestWatchConnLookupRoutesByID covers the demultiplexer that lets LOOKUP share
// the persistent director connection (#878): replies are matched by the echoed
// request id, so they may arrive in any order.
func TestWatchConnLookupRoutesByID(t *testing.T) {
	wc := &watchConn{}

	ids := []string{"1", "2", "3"}
	chans := make(map[string]chan string, len(ids))
	for _, id := range ids {
		chans[id] = wc.awaitReply(id)
	}

	// Deliver in reverse order — arrival order must not matter.
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		if !wc.deliver(id, "HOST\t"+id+"\t10.0.0."+id+"\t993\tssd") {
			t.Fatalf("deliver(%s) found no waiter", id)
		}
	}

	for _, id := range ids {
		select {
		case line := <-chans[id]:
			res, err := proto.ParseLookupReply(line)
			if err != nil {
				t.Fatalf("id %s: %v", id, err)
			}
			if want := "10.0.0." + id + ":993"; res.Addr != want {
				t.Fatalf("id %s: addr = %q, want %q", id, res.Addr, want)
			}
		default:
			t.Fatalf("id %s: no reply routed", id)
		}
	}
}

func TestWatchConnDeliverWithoutWaiter(t *testing.T) {
	wc := &watchConn{}
	// An id nobody waits for is a reply whose caller already timed out. Dropping
	// it must not panic and must be reported as unclaimed so the read loop can
	// fall through to its push handling.
	if wc.deliver("42", "HOST\t42\t10.0.0.1\t993") {
		t.Fatal("deliver reported a waiter where none was registered")
	}
}

func TestWatchConnForgetReply(t *testing.T) {
	wc := &watchConn{}
	wc.awaitReply("7")
	wc.forgetReply("7")
	if wc.deliver("7", "HOST\t7\t10.0.0.1\t993") {
		t.Fatal("a forgotten id still had a waiter")
	}
}

// TestWatchConnFailPendingWakesWaiters is the recovery path: when the connection
// dies, an in-flight lookup must be released immediately rather than waiting out
// its timeout, so the caller can retry on a fresh dial.
func TestWatchConnFailPendingWakesWaiters(t *testing.T) {
	wc := &watchConn{}
	ch := wc.awaitReply("9")

	var wg sync.WaitGroup
	wg.Add(1)
	var closed bool
	go func() {
		defer wg.Done()
		select {
		case _, ok := <-ch:
			closed = !ok
		case <-time.After(2 * time.Second):
		}
	}()

	wc.failPending()
	wg.Wait()

	if !closed {
		t.Fatal("failPending did not release the waiting lookup")
	}
}

// TestParseLookupReplyOutcomes pins the reply taxonomy the shared path depends
// on, in particular that a confirmed-kick hold stays retryable.
func TestParseLookupReplyOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantAddr string
		wantHold bool
		wantErr  bool
	}{
		{name: "host with tag", line: "HOST\t1\t10.0.0.5\t993\tssd", wantAddr: "10.0.0.5:993"},
		{name: "host without tag", line: "HOST\t1\t10.0.0.5\t993", wantAddr: "10.0.0.5:993"},
		{name: "kick hold is retryable", line: "FAIL\t1\treason=killing", wantHold: true, wantErr: true},
		{name: "no backends", line: "FAIL\t1\treason=no-backends", wantErr: true},
		{name: "malformed host", line: "HOST\t1", wantErr: true},
		{name: "unknown verb", line: "WAT\t1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := proto.ParseLookupReply(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for %q", tc.line)
				}
				if got := errors.Is(err, proto.ErrLookupHold); got != tc.wantHold {
					t.Fatalf("ErrLookupHold = %v, want %v (err=%v)", got, tc.wantHold, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Addr != tc.wantAddr {
				t.Fatalf("addr = %q, want %q", res.Addr, tc.wantAddr)
			}
		})
	}
}

func TestApplyBackendPort(t *testing.T) {
	tests := []struct {
		name string
		port int
		addr string
		want string
	}{
		{name: "override applied", port: 10143, addr: "10.0.0.7:993", want: "10.0.0.7:10143"},
		{name: "no override configured", port: 0, addr: "10.0.0.7:993", want: "10.0.0.7:993"},
		{name: "unsplittable address passes through", port: 10143, addr: "garbage", want: "garbage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{opts: Options{BackendPort: tc.port}}
			if got := s.applyBackendPort(tc.addr); got != tc.want {
				t.Fatalf("applyBackendPort(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestLookupRequestLineEscapesUser(t *testing.T) {
	line := proto.LookupRequestLine("5", "user\twith\ttabs@d", "ssd", "imap")
	fields := strings.Split(line, "\t")
	// Verb, id, username, tag, protocol — the username must not have split into
	// extra fields, or the director would parse the wrong tag.
	if len(fields) != 5 {
		t.Fatalf("got %d fields, want 5: %q", len(fields), line)
	}
	if fields[0] != "LOOKUP" || fields[1] != "5" || fields[3] != "ssd" || fields[4] != "imap" {
		t.Fatalf("unexpected framing: %q", line)
	}
}
