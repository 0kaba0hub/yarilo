package imap_test

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// countingBackend counts how often a folder is opened, which is what an
// account-wide quota sweep does once per folder.
type countingBackend struct {
	mailbox.IndexBackend
	opens *int64
}

func (b countingBackend) OpenUser(u *mailbox.UserInfo) mailbox.UserIndex {
	return countingIndex{UserIndex: b.IndexBackend.OpenUser(u), opens: b.opens}
}

type countingIndex struct {
	mailbox.UserIndex
	opens *int64
}

func (i countingIndex) OpenFolder(name string, uidValidity uint32) (*mailbox.Folder, error) {
	atomic.AddInt64(i.opens, 1)
	return i.UserIndex.OpenFolder(name, uidValidity)
}

// An expunge must not re-count the whole account.
//
// Counting usage opens every folder of the user: measured at about 24 us per
// folder, so 1.01 ms across the 42 folders of a real mailbox regardless of how
// much mail is in them. Run on every committed change -- which is what the
// quota-warning path did -- the cost of an expunge is set by how many folders
// the account has, and fifty clients expunging in one folder produce fifty
// sweeps and some two thousand folder opens per round (#1548).
//
// The number of folders is the input that distinguishes: a session that counts
// from its own delta opens folders a number of times that does not move when
// the account grows a folder, and a session that sweeps opens one per folder
// per expunge.
func TestAnExpungeDoesNotOpenEveryFolderOfTheAccount(t *testing.T) {
	const folders = 30

	var opens int64
	dir := t.TempDir()
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    countingBackend{IndexBackend: file.New(), opens: &opens},
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:     &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000000"},
		QuotaPolicy: quota.Policy{
			Warnings: []quota.Warning{{Name: "over90", Resource: "storage", Percentage: 90}},
		},
		QuotaWarner: quotawarn.New("", 5),
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < folders; i++ {
		if err := c.Create(fmt.Sprintf("F%d", i), nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	const msgs = 10
	for i := 0; i < msgs; i++ {
		body := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		ac := c.Append("INBOX", int64(len(body)), nil)
		if _, err := ac.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := ac.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	var set imaplib.SeqSet
	set.AddRange(1, uint32(msgs))
	if err := c.Store(set, &imaplib.StoreFlags{
		Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt64(&opens, 0)
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	got := atomic.LoadInt64(&opens)

	// One sweep per expunged message would be folders*msgs; the budget is
	// generous enough that ordinary per-command opens pass and a sweep cannot.
	if budget := int64(folders * msgs / 2); got > budget {
		t.Errorf("expunging %d messages opened folders %d times on an account with %d folders: that is the account-wide sweep, not the delta (budget %d)",
			msgs, got, folders, budget)
	}
}
