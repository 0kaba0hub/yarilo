package imap_test

import (
	"bytes"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// slowFlagBackend is maildir with the flag-write pass made visibly slow, so the
// timing line has something to fail to notice.
type slowFlagBackend struct {
	mailbox.MailboxBackend
	delay time.Duration
}

type slowFlagUser struct {
	mailbox.UserMailbox
	delay time.Duration
}

func (b *slowFlagBackend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	return &slowFlagUser{UserMailbox: b.MailboxBackend.OpenUser(u), delay: b.delay}
}

func (u *slowFlagUser) WriteFlagsMulti(folder string, writes []mailbox.FlagWrite) []mailbox.FlagWriteResult {
	time.Sleep(u.delay)
	return u.UserMailbox.(mailbox.FlagWriterMulti).WriteFlagsMulti(folder, writes)
}

// The timing line has to span the storage write, which runs in a defer after
// the point the line used to be printed at. It did not, and a measured run came
// back with a 2.7s ceiling for stalls of 16 to 18 seconds: the part that could
// have explained them was outside the window by construction (#1646).
func TestStoreTimingSpansTheStorageWrite(t *testing.T) {
	const delay = 300 * time.Millisecond
	dir := t.TempDir()

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	opts := imapserver.Options{
		Mailbox:  &slowFlagBackend{MailboxBackend: maildir.New(), delay: delay},
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:     &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=100000"},
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

	logged.Reset()
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}
	if err := c.Store(imap.SeqSetNum(1), store, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}

	total, ok := lastTimingField(logged.String(), "total_ms")
	if !ok {
		t.Fatal("no store timing line was logged")
	}
	if want := delay.Milliseconds(); total < want {
		t.Errorf("the timing line reported %dms for a STORE whose storage write alone slept %dms: "+
			"the window closes before the write and cannot see it", total, want)
	}
	if rename, ok := lastTimingField(logged.String(), "rename_ms"); !ok || rename < delay.Milliseconds() {
		t.Errorf("rename_ms = %dms, want at least %dms: the tail is not split where the cures differ",
			rename, delay.Milliseconds())
	}
}

// lastTimingField reads one number out of the last "store timing" line.
func lastTimingField(out, field string) (int64, bool) {
	var found string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "store timing") {
			found = line
		}
	}
	if found == "" {
		return 0, false
	}
	at := strings.Index(found, `"`+field+`":`)
	if at < 0 {
		return 0, false
	}
	rest := found[at+len(field)+3:]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest[:end]), 10, 64)
	return n, err == nil
}
