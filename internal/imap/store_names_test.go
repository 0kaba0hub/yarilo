package imap_test

import (
	"net"
	"sync/atomic"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// nameCountingBackend counts which of the two name-recording calls a command makes.
type nameCountingBackend struct {
	mailbox.IndexBackend
	single, batch, named *atomic.Int64
}

type nameCountingIndex struct {
	mailbox.UserIndex
	single, batch, named *atomic.Int64
}

func (b *nameCountingBackend) OpenUser(u *mailbox.UserInfo) mailbox.UserIndex {
	return &nameCountingIndex{UserIndex: b.IndexBackend.OpenUser(u),
		single: b.single, batch: b.batch, named: b.named}
}

func (u *nameCountingIndex) UpdateFilename(folderID uint64, uid uint32, name string) error {
	u.single.Add(1)
	u.named.Add(1)
	return u.UserIndex.UpdateFilename(folderID, uid, name)
}

func (u *nameCountingIndex) UpdateFilenames(folderID uint64, names map[uint32]string) error {
	u.batch.Add(1)
	u.named.Add(int64(len(names)))
	return u.UserIndex.(mailbox.FilenameWriterMulti).UpdateFilenames(folderID, names)
}

// allSeqs is 1..n as a sequence set.
func allSeqs(n int) imap.NumSet {
	nums := make([]uint32, 0, n)
	for i := 1; i <= n; i++ {
		nums = append(nums, uint32(i))
	}
	return imap.SeqSetNum(nums...)
}

// A STORE over many messages records their names once, not once each.
//
// Every one of those calls takes the index's exclusive lock. Measured at 121ms
// a name on a contended folder, a 41-message STORE spent 17.7 of its 18.1
// seconds here, and imaptest reported that same command stalled for 18 (#1646).
func TestOneStoreRecordsItsNamesInOneCall(t *testing.T) {
	dir := t.TempDir()
	var single, batch, named atomic.Int64
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    &nameCountingBackend{IndexBackend: file.New(), single: &single, batch: &batch, named: &named},
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
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	const n = 12
	for i := 0; i < n; i++ {
		body := []byte("From: a@b.test\r\nSubject: m\r\n\r\nbody\r\n")
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
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	single.Store(0)
	batch.Store(0)
	named.Store(0)
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}
	if err := c.Store(allSeqs(n), store, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}

	if got := batch.Load(); got != 1 {
		t.Errorf("the STORE recorded its names in %d batched calls, want 1", got)
	}
	if got := single.Load(); got != 0 {
		t.Errorf("the STORE also made %d single-name calls, each taking the index lock", got)
	}
	// A batch of one name would satisfy the counts above and record nothing:
	// on maildir every message in this STORE gains \Seen and is renamed.
	if got := named.Load(); got != n {
		t.Errorf("the calls carried %d names for a %d-message STORE", got, n)
	}
}
