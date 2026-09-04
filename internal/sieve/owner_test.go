package sieve

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

type recordingLocker struct {
	mu     sync.Mutex
	owners []string
}

func (l *recordingLocker) Lock(_ context.Context, _, owner string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owners = append(l.owners, owner)
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

// A script write announces the user and the session that made it.
//
// It announced "sieve:<pid>": a word and a pid, no user and no session, so a
// BUSY on a script file named nothing an operator could act on. The id travels
// in the context because the store interface has no place for one -- managesieve
// puts its session there, delivery its connection's (#1672).
func TestAScriptWriteNamesTheUserAndTheSession(t *testing.T) {
	rec := &recordingLocker{}
	ss := &FsScriptStore{DefaultName: FallbackDefaultName, Locker: rec}
	ctx := locks.WithID(context.Background(), "4hbQ6j4PCJz1RCkh1Fr")
	if err := ss.SaveScript(ctx, "u1@example.com", t.TempDir(), "personal", []byte("keep;\r\n")); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.owners) == 0 {
		t.Fatal("the write took no lock, so this measures nothing")
	}
	for _, o := range rec.owners {
		if n := len(strings.Split(o, "/")); n != 4 {
			t.Errorf("a script write announced itself as %q (%d segments, want 4)", o, n)
		}
		if !strings.Contains(o, "u1@example.com") {
			t.Errorf("owner %q does not name the user whose script it is writing", o)
		}
		if !strings.HasSuffix(o, "/4hbQ6j4PCJz1RCkh1Fr") {
			t.Errorf("owner %q does not carry the session's id", o)
		}
	}
}
