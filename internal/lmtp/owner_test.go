package lmtp

import (
	"context"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// recordingLocker keeps every owner string handed to the lock service and grants
// everything: what is under test is what the caller says about itself.
type recordingLocker struct {
	mu     sync.Mutex
	owners map[string]struct{}
}

func (l *recordingLocker) Lock(_ context.Context, _, owner string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owners == nil {
		l.owners = map[string]struct{}{}
	}
	l.owners[owner] = struct{}{}
	return locks.Lock{ID: "x"}, nil
}

func (l *recordingLocker) LockShared(ctx context.Context, r, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, owner, ttl)
}
func (l *recordingLocker) Unlock(context.Context, string) error               { return nil }
func (l *recordingLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *recordingLocker) HoldsResource(string) bool                          { return false }
func (l *recordingLocker) Close() error                                       { return nil }
func (l *recordingLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *recordingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *recordingLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func (l *recordingLocker) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.owners))
	for o := range l.owners {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// A delivery holds its locks under an identifier of its own.
//
// A delivery has no IMAP session, which is not the same as having no identity:
// it announced "<proc>/<pid>/<user>" and an operator reading held_by could not
// tell one delivery from another, or from anything else in the process. The
// connection mints one now, the way every other entry point does (#1670).
func TestADeliveryNamesItself(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingLocker{}
	srv := New(Options{
		Hostname: "lmtp.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader: true,
			ReadTimeout:       5,
			WriteTimeout:      5,
		},
		Mailbox:  maildir.New(maildir.WithLocker(rec)),
		Index:    fileindex.New(fileindex.WithLocker(rec)),
		Locker:   rec,
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, user)
			return &mailbox.UserInfo{Username: user, Home: home, Driver: "maildir"}, nil
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if len(resp) == 0 || resp[0][0] != '2' {
		t.Fatalf("delivery not accepted: %v", resp)
	}

	seen := rec.seen()
	if len(seen) == 0 {
		t.Fatal("no lock was taken, so this measures nothing")
	}
	for _, o := range seen {
		if n := len(strings.Split(o, "/")); n != 4 {
			t.Errorf("a delivery announced itself as %q (%d segments, want 4) -- "+
				"held_by cannot tell this delivery from any other", o, n)
		}
	}
	if len(seen) != 1 {
		t.Errorf("one delivery reached the lock service under %d names:\n  %v", len(seen), seen)
	}
}
