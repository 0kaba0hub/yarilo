package director

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func TestUserKickDelay(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"default", 0, 2 * time.Second},
		{"explicit", 5 * time.Second, 5 * time.Second},
		{"negative disables", -1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &Options{UserKickDelay: tc.in}
			if got := o.userKickDelay(); got != tc.want {
				t.Fatalf("userKickDelay(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMaxParallelKicks(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"default", 0, 100},
		{"explicit", 50, 50},
		{"negative disables batching", -1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &Options{MaxParallelKicks: tc.in}
			if got := o.maxParallelKicks(); got != tc.want {
				t.Fatalf("maxParallelKicks(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// recordConn is a net.Conn whose every Write pushes the written bytes onto a
// channel, letting a test count kicks by blocking-read (no sleep in test code)
// while the batched kicker paces itself with real pauses in a goroutine.
type recordConn struct{ lines chan string }

func (c *recordConn) Write(b []byte) (int, error)        { c.lines <- string(b); return len(b), nil }
func (c *recordConn) Read([]byte) (int, error)           { return 0, io.EOF }
func (c *recordConn) Close() error                       { return nil }
func (c *recordConn) LocalAddr() net.Addr                { return nil }
func (c *recordConn) RemoteAddr() net.Addr               { return nil }
func (c *recordConn) SetDeadline(time.Time) error        { return nil }
func (c *recordConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *recordConn) SetWriteDeadline(t time.Time) error { return nil }

// TestKickSessionsForBackend_BatchesAllLocal: with max_parallel_kicks smaller
// than the session count, every local session is still kicked (across batches),
// and a co-located remote replica (cl == nil) is skipped without a nil deref.
func TestKickSessionsForBackend_BatchesAllLocal(t *testing.T) {
	const nLocal = 4
	s := NewWithOptions(Options{AntiEntropyInterval: -1, MaxParallelKicks: 2})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.5", Port: 10143, Tag: "a", Up: true, Vhosts: 100})

	conn := &recordConn{lines: make(chan string, nLocal)}
	s.sessRecMu.Lock()
	s.sessByBE["10.0.0.5"] = map[string]bool{}
	for i := 0; i < nLocal; i++ {
		id := fmt.Sprintf("sid%d", i)
		s.sessById[id] = &sessionRec{id: id, user: "u", backend: "10.0.0.5", proto: "imap", cl: &client{conn: conn}}
		s.sessByBE["10.0.0.5"][id] = true
	}
	s.sessRecMu.Unlock()
	// One remote replica on the same backend — must be skipped by the kicker.
	s.applyRemoteSessionOpen([]string{"remote1", "u", "10.0.0.5", "imap"}, "10.0.0.99@run1")

	s.kickSessionsForBackend("10.0.0.5")

	// Read exactly nLocal kicks (blocking; the batched goroutine paces itself).
	got := 0
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for got < nLocal {
		select {
		case line := <-conn.lines:
			if !strings.HasPrefix(line, "USER-KICKED\t") {
				t.Fatalf("unexpected kick line %q", line)
			}
			got++
		case <-deadline.C:
			t.Fatalf("only %d/%d kicks delivered before timeout", got, nLocal)
		}
	}
	// No extra kick from the remote replica.
	select {
	case line := <-conn.lines:
		t.Fatalf("unexpected extra kick %q (remote must be skipped)", line)
	case <-time.After(200 * time.Millisecond):
	}

	if total, _ := s.sessionCounts(); total["10.0.0.5"] != 0 {
		t.Fatalf("kick must clear the registry (local+remote), got %v", total)
	}
}
