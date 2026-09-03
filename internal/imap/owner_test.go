package imap_test

import (
	"context"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// recordingLocker keeps every owner string handed to the lock service. Nothing
// is ever held: what is under test is what the callers say about themselves, and
// a locker that grants nothing exercises more paths than one that blocks.
type recordingLocker struct {
	mu     sync.Mutex
	owners map[string]struct{}
}

func (l *recordingLocker) note(owner string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owners == nil {
		l.owners = map[string]struct{}{}
	}
	l.owners[owner] = struct{}{}
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

func (l *recordingLocker) Lock(_ context.Context, _, owner string, _ time.Duration) (locks.Lock, error) {
	l.note(owner)
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

// One session identifies itself the same way on every path that takes a lock.
//
// Not "the call sites all call one function", which is visible by eye and stays
// true until somebody adds a seventh site. What is asserted is the property an
// operator depends on: everything a session does reaches the service under one
// byte-identical name. There were six spellings, and held_by said three
// different things about one holder (#1647).
//
// The session id is planted, because a real one comes from the login proxy's
// preamble and a test connection has none -- which made every spelling here
// collapse to the sessionless form, so a regression dropping the session
// passed unnoticed. It does not any more (#1652).
func TestASessionTakesEveryLockUnderOneName(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingLocker{}
	imapserver.SetTestSessionID("4hbQ6j4PCJz1RCkh1Fr")
	defer imapserver.SetTestSessionID("")
	opts := imapserver.Options{
		// The locker goes into the storage backends too, not only the session's
		// own stores: without it the driver and the index take no lock at all
		// and this test saw one spelling out of three (#1652).
		Mailbox:  maildir.New(maildir.WithLocker(rec)),
		Index:    file.New(file.WithLocker(rec)),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:     &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000000"},
		Locker:   rec,
		// An owner-templated namespace: its handle takes a different road to
		// storage -- the owner comes from userdb, which knows nothing of
		// sessions -- and that is the road that produced a second spelling of
		// one holder (#1652).
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/',
				Location: "maildir:" + dir + "/shared/%u", List: imapserver.ListYes},
		},
		UserdbLookup: func(_ context.Context, name string) (*mailbox.UserInfo, error) {
			return &mailbox.UserInfo{Username: name, Home: dir + "/" + name, Driver: "maildir"}, nil
		},
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	defer ln.Close() //nolint:errcheck
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Paths that take a lock under different owners before this change: the
	// driver (rename), the index (write), subscriptions, and the folder list.
	body := []byte("From: a@b.test\r\nSubject: one\r\n\r\nbody\r\n")
	ac := c.Append("INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}
	if err := c.Store(imap.SeqSetNum(1), store, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := c.Subscribe("INBOX").Wait(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := c.List("", "*", nil).Collect(); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Through the owner-templated namespace: its own maildir under dir/%u, so
	// Save takes the folder lock with an owner built from the userdb answer
	// rather than from the session.
	oc := c.Append("user/user@test.com/INBOX", int64(len(body)), nil)
	if _, err := oc.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := oc.Close(); err != nil {
		t.Fatalf("append through the owner namespace: %v", err)
	}
	if _, err := oc.Wait(); err != nil {
		t.Fatalf("append through the owner namespace: %v", err)
	}
	if err := c.Logout().Wait(); err != nil {
		t.Fatal(err)
	}

	seen := rec.seen()
	if len(seen) == 0 {
		t.Fatal("no lock was taken, so this measures nothing")
	}
	if len(seen) != 1 {
		t.Errorf("one session reached the lock service under %d names:\n  %v", len(seen), seen)
	}
}
