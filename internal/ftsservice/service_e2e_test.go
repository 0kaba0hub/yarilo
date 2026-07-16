//go:build flatcurve

package ftsservice

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/fts/flatcurve"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/ftsproto"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

const testUser = "u@test.com"

var testMbox = fts.MailboxRef{Name: "INBOX", UIDValidity: 1}

func newTestService(t *testing.T) (*Service, mailbox.UserMailbox, mailbox.UserIndex) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	chain, err := language.NewChain(language.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Engine:      flatcurve.New(flatcurve.Options{}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil },
		Chain:       chain,
		CommitLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck

	info := resolver.UserInfo(testUser, "")
	box := mb.OpenUser(info)
	uidx := idx.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close(); uidx.Close() }) //nolint:errcheck
	return svc, box, uidx
}

func saveMessage(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, uid uint32, body string) {
	t.Helper()
	raw := "From: a@test.com\r\nSubject: note " + body + "\r\n\r\n" + body + "\r\n"
	f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	name, err := box.Save(testMbox.Name, strings.NewReader(raw), uid, int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := uidx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: uid, Filename: name, Size: uint32(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
}

func waitIndexed(t *testing.T, svc ftsproto.Service, uid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		last, _, err := svc.Status(testUser, testMbox)
		if err == nil && last >= uid {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("indexing did not reach the expected checkpoint")
}

func lookupWord(word string) fts.Query {
	return fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{word}}}}},
		AndTerms: true,
	}
}

func TestServiceEndToEnd(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "wolves howl nightly")
	saveMessage(t, box, uidx, 2, "quiet meadow")
	saveMessage(t, box, uidx, 3, "wolves rest")

	if err := svc.Index(testUser, testMbox, 3, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 3)

	res, err := svc.Lookup(testUser, testMbox, lookupWord("wolv")) // stemmed "wolves"
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 2 || res.Definite[0] != 1 || res.Definite[1] != 3 {
		t.Fatalf("lookup = %v, want [1 3]", res.Definite)
	}

	// Expunge is synchronous.
	if err := svc.Expunge(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Lookup(testUser, testMbox, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 3 {
		t.Fatalf("after expunge = %v, want [3]", res.Definite)
	}

	// Rescan reconciles and re-queues nothing when consistent.
	if err := svc.Rescan(testUser, testMbox); err != nil {
		t.Fatal(err)
	}
	if err := svc.Optimize(testUser); err != nil {
		t.Fatal(err)
	}
}

func TestServiceWireRoundTrip(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "remote lookup target")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ftsproto.Serve(ln, svc) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	cl, err := ftsproto.Dial(ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if err := cl.Prepend(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, cl, 1)

	res, err := cl.Lookup(testUser, testMbox, lookupWord("remot"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 1 {
		t.Fatalf("wire lookup = %v, want [1]", res.Definite)
	}
	last, sum, err := cl.Status(testUser, testMbox)
	if err != nil || last != 1 || sum == 0 {
		t.Fatalf("wire status = %d/%d/%v", last, sum, err)
	}
	if err := cl.Expunge(testUser, testMbox, 1); err != nil {
		t.Fatal(err)
	}
	if err := cl.Rescan(testUser, testMbox); err != nil {
		t.Fatal(err)
	}
	if err := cl.Optimize(testUser); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsDriftRebuild(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "driftcheck")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)

	// Forge a checkpoint with a foreign settings checksum: the next index
	// job must rebuild the mailbox and restore a working index.
	h, err := svc.handle(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ui.SetCheckpoint(testMbox, 1, 12345); err != nil {
		t.Fatal(err)
	}
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)
	res, err := svc.Lookup(testUser, testMbox, lookupWord("driftcheck"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Fatalf("after drift rebuild = %v, want [1]", res.Definite)
	}
}
