package imap_test

import (
	"net"
	"sync"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A FETCH shares the folder key only when it writes no flag. The wiring is one
// line, otherwise reachable only by racing two clients (#1673).
func TestFetchSharesTheKeyOnlyWhenItWritesNothing(t *testing.T) {
	dir := t.TempDir()
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:     &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000000"},
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	defer ln.Close() //nolint:errcheck

	var mu sync.Mutex
	var seen []msgcache.Options
	defer imapserver.SetEnvCacheObserver(func(o msgcache.Options) {
		mu.Lock()
		seen = append(seen, o)
		mu.Unlock()
	})()

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

	fetch := func(peek bool) msgcache.Options {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		fo := &imap.FetchOptions{
			Envelope:    true,
			BodySection: []*imap.FetchItemBodySection{{Peek: peek}},
		}
		if _, err := c.Fetch(imap.SeqSetNum(1), fo).Collect(); err != nil {
			t.Fatalf("fetch(peek=%v): %v", peek, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 1 {
			t.Fatalf("the envelope cache was opened %d times, want 1", len(seen))
		}
		return seen[0]
	}

	if o := fetch(true); !o.Shared {
		t.Error("a peeking FETCH took the folder key exclusively: concurrent readers serialise")
	}
	if o := fetch(false); o.Shared {
		t.Error("a FETCH that sets \\Seen shared the folder key: a reader can run beside the flag write")
	}
}
