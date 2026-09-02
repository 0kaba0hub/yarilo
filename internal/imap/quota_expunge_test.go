package imap_test

import (
	"context"
	"net"
	"strconv"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// startCloningServer is the enforcing harness with the clone mirror as the only
// post-commit reader and no warnings configured -- the deployed shape. Warnings
// would hide what this file measures: their baseline counts before the expunge
// and leaves a fresh total behind, so the delta would have a base no deployment
// gives it.
func startCloningServer(t *testing.T, dir string) (*imapclient.Client, dict.Dict) {
	t.Helper()
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	opts := imapserver.Options{
		Mailbox:     maildir.New(),
		Index:       file.New(),
		Resolver:    &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:        &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=100000"},
		QuotaEngine: true,
		QuotaClone:  quota.NewClone([]dict.Dict{d}),
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c, d
}

func appendOne(t *testing.T, c *imapclient.Client, subject string) {
	t.Helper()
	body := []byte("From: a@b.test\r\nSubject: " + subject + "\r\n\r\nbody\r\n")
	ac := c.Append("INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	// Close only ends the literal; without the tagged response the counters are
	// read before the server has run the command.
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("append %s: %v", subject, err)
	}
}

func mirroredMessages(t *testing.T, d dict.Dict) int64 {
	t.Helper()
	vs, found, err := d.Lookup(context.Background(),
		&dict.OpSettings{Username: "user@test.com"}, "priv/quota/messages")
	if err != nil || !found || len(vs) == 0 {
		t.Fatalf("the clone mirror holds no message count: found=%v err=%v", found, err)
	}
	n, perr := strconv.ParseInt(string(vs[0]), 10, 64)
	if perr != nil {
		t.Fatalf("mirrored count %q: %v", vs[0], perr)
	}
	return n
}

// The floor: a session that only deletes counts the account once, and every
// expunge after that is free. Measured, not aspired to -- a field run showed
// 0.7 counts per session, which is this floor rather than a saving left on the
// table (#1637).
//
// Why one count is irreducible is worth stating, because it is what stops the
// next reader hunting a saving that does not exist: the delta carries a total
// forward, and a session that has never counted has no total to carry. The
// mirror wants an absolute number, and an absolute number without a walk is not
// a cheaper walk -- there is no such thing.
//
// The session doing the deleting is deliberately not the one that wrote: that
// is the shape with no recent count of its own, and the only one where the
// first expunge can be seen paying.
func TestADeleteOnlySessionCountsOnceAndThenNotAtAll(t *testing.T) {
	dir := t.TempDir()
	w, _ := startCloningServer(t, dir)
	for _, s := range []string{"one", "two", "three"} {
		appendOne(t, w, s)
	}
	if err := w.Logout().Wait(); err != nil {
		t.Fatal(err)
	}

	c, d := startCloningServer(t, dir)
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}

	expunge := func(seq uint32) float64 {
		t.Helper()
		if err := c.Store(imap.SeqSetNum(seq), store, nil).Close(); err != nil {
			t.Fatalf("store: %v", err)
		}
		before := walks("post-write")
		if err := c.Expunge().Close(); err != nil {
			t.Fatalf("expunge: %v", err)
		}
		return walks("post-write") - before
	}

	if got := expunge(1); got != 1 {
		t.Errorf("the first expunge of a delete-only session counted %v times, want 1", got)
	}
	if got := mirroredMessages(t, d); got != 2 {
		t.Fatalf("the mirror holds %d messages after one of three was removed, want 2", got)
	}

	for i, seq := range []uint32{1, 1} {
		if got := expunge(seq); got != 0 {
			t.Errorf("expunge %d after the first counted %v times: the delta stopped carrying the "+
				"total forward, and every delete walks every folder again", i+2, got)
		}
	}
	// The zeros above mean nothing on their own: a folder with nothing left to
	// remove also counts zero times. The mirror is what shows the deltas landed.
	if got := mirroredMessages(t, d); got != 0 {
		t.Errorf("the mirror holds %d messages after all three were removed, want 0 -- "+
			"the free expunges never reached the total", got)
	}
}
