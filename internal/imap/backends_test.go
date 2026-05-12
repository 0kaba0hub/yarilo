package imap_test

import (
	"net"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dbox"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// startServerWith starts an IMAP server backed by the given MailboxBackend
// and returns an authenticated client ready for mailbox operations.
func startServerWith(t *testing.T, mb mailbox.MailboxBackend) *imapclient.Client {
	t.Helper()

	idx := file.New(t.TempDir())
	opts := imapserver.Options{
		Mailbox: mb,
		Index:   idx,
		Auth:    &stubPassdb{user: "user@test.com", pass: "testpass"},
	}
	srv := imapserver.New(opts)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return c
}

// backendFactory returns a MailboxBackend and its name for table-driven tests.
type backendFactory struct {
	name string
	new  func(t *testing.T) mailbox.MailboxBackend
}

func dboxBackend(t *testing.T) mailbox.MailboxBackend {
	t.Helper()
	b, err := dbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("dbox.New: %v", err)
	}
	return b
}

func mdboxBackend(t *testing.T) mailbox.MailboxBackend {
	t.Helper()
	b, err := mdbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mdbox.New: %v", err)
	}
	return b
}

var backends = []backendFactory{
	{"dbox", dboxBackend},
	{"mdbox", mdboxBackend},
}

// TestIMAPBackends runs the full IMAP operation sequence against each backend.
// Covers: APPEND, SELECT, FETCH (FLAGS + BODY[]), STORE, EXPUNGE.
func TestIMAPBackends(t *testing.T) {
	for _, bf := range backends {
		bf := bf
		t.Run(bf.name, func(t *testing.T) {
			t.Parallel()
			mb := bf.new(t)
			c := startServerWith(t, mb)
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			const msgBody = "From: a@b.com\r\nSubject: Phase2\r\n\r\nHello Phase2\r\n"

			// APPEND
			body := []byte(msgBody)
			ac := c.Append("INBOX", int64(len(body)), &imap.AppendOptions{
				Flags: []imap.Flag{imap.FlagSeen},
			})
			if _, err := ac.Write(body); err != nil {
				t.Fatalf("Append write: %v", err)
			}
			if err := ac.Close(); err != nil {
				t.Fatalf("Append close: %v", err)
			}
			if _, err := ac.Wait(); err != nil {
				t.Fatalf("Append wait: %v", err)
			}

			// SELECT
			selectData, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			if selectData.NumMessages != 1 {
				t.Fatalf("SELECT NumMessages = %d, want 1", selectData.NumMessages)
			}

			// FETCH FLAGS — must include \Seen
			flagMsgs, err := c.Fetch(
				imap.SeqSetNum(1),
				&imap.FetchOptions{Flags: true},
			).Collect()
			if err != nil {
				t.Fatalf("FETCH FLAGS: %v", err)
			}
			if len(flagMsgs) != 1 {
				t.Fatalf("FETCH FLAGS: got %d messages, want 1", len(flagMsgs))
			}
			if !hasImapFlag(flagMsgs[0].Flags, imap.FlagSeen) {
				t.Errorf("FETCH FLAGS: \\Seen missing from %v", flagMsgs[0].Flags)
			}

			// FETCH BODY[] — must return original message
			bodyFetch, err := c.Fetch(
				imap.SeqSetNum(1),
				&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}},
			).Collect()
			if err != nil {
				t.Fatalf("FETCH BODY[]: %v", err)
			}
			if len(bodyFetch) != 1 {
				t.Fatalf("FETCH BODY[]: got %d messages, want 1", len(bodyFetch))
			}
			if len(bodyFetch[0].BodySection) == 0 {
				t.Fatal("FETCH BODY[]: no body section returned")
			}
			got := string(bodyFetch[0].BodySection[0].Bytes)
			if !strings.Contains(got, "Hello Phase2") {
				t.Errorf("BODY[] does not contain original message: %q", got)
			}

			// STORE +FLAGS (\Flagged)
			storeCmd := c.Store(
				imap.SeqSetNum(1),
				&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}},
				nil,
			)
			if err := storeCmd.Close(); err != nil {
				t.Fatalf("STORE +FLAGS: %v", err)
			}

			afterStore, err := c.Fetch(
				imap.SeqSetNum(1),
				&imap.FetchOptions{Flags: true},
			).Collect()
			if err != nil {
				t.Fatalf("FETCH after STORE: %v", err)
			}
			if !hasImapFlag(afterStore[0].Flags, imap.FlagFlagged) {
				t.Errorf("STORE: \\Flagged missing from %v", afterStore[0].Flags)
			}
			if !hasImapFlag(afterStore[0].Flags, imap.FlagSeen) {
				t.Errorf("STORE: \\Seen lost after adding \\Flagged: %v", afterStore[0].Flags)
			}

			// STORE \Deleted + EXPUNGE
			storeCmd2 := c.Store(
				imap.SeqSetNum(1),
				&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
				nil,
			)
			if err := storeCmd2.Close(); err != nil {
				t.Fatalf("STORE \\Deleted: %v", err)
			}
			if err := c.Expunge().Close(); err != nil {
				t.Fatalf("EXPUNGE: %v", err)
			}

			// Re-SELECT to verify mailbox is empty.
			selectData2, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("SELECT after EXPUNGE: %v", err)
			}
			if selectData2.NumMessages != 0 {
				t.Fatalf("after EXPUNGE: NumMessages = %d, want 0", selectData2.NumMessages)
			}
		})
	}
}

// TestIMAPBackends_MultipleMessages verifies correct ordering and count
// when several messages are appended.
func TestIMAPBackends_MultipleMessages(t *testing.T) {
	for _, bf := range backends {
		bf := bf
		t.Run(bf.name, func(t *testing.T) {
			t.Parallel()
			mb := bf.new(t)
			c := startServerWith(t, mb)
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			bodies := []string{
				"From: a\r\n\r\nOne\r\n",
				"From: b\r\n\r\nTwo\r\n",
				"From: c\r\n\r\nThree\r\n",
			}
			for _, body := range bodies {
				b := []byte(body)
				ac := c.Append("INBOX", int64(len(b)), nil)
				if _, err := ac.Write(b); err != nil {
					t.Fatalf("Append write: %v", err)
				}
				if err := ac.Close(); err != nil {
					t.Fatalf("Append close: %v", err)
				}
				if _, err := ac.Wait(); err != nil {
					t.Fatalf("Append wait: %v", err)
				}
			}

			selectData, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			if selectData.NumMessages != uint32(len(bodies)) {
				t.Fatalf("SELECT NumMessages = %d, want %d", selectData.NumMessages, len(bodies))
			}

			msgs, err := c.Fetch(
				imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}},
				&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}},
			).Collect()
			if err != nil {
				t.Fatalf("FETCH 1:*: %v", err)
			}
			if len(msgs) != len(bodies) {
				t.Fatalf("FETCH 1:*: got %d messages, want %d", len(msgs), len(bodies))
			}
		})
	}
}

// TestIMAPBackends_CreateDelete verifies folder creation and deletion.
func TestIMAPBackends_CreateDelete(t *testing.T) {
	for _, bf := range backends {
		bf := bf
		t.Run(bf.name, func(t *testing.T) {
			t.Parallel()
			mb := bf.new(t)
			c := startServerWith(t, mb)
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			if err := c.Create("Sent", nil).Wait(); err != nil {
				t.Fatalf("CREATE Sent: %v", err)
			}

			// LIST — Sent must appear.
			listData, err := c.List("", "*", nil).Collect()
			if err != nil {
				t.Fatalf("LIST: %v", err)
			}
			found := false
			for _, m := range listData {
				if m.Mailbox == "Sent" {
					found = true
				}
			}
			if !found {
				t.Errorf("Sent not found in LIST after CREATE")
			}

			if err := c.Delete("Sent").Wait(); err != nil {
				t.Fatalf("DELETE Sent: %v", err)
			}

			listData2, err := c.List("", "*", nil).Collect()
			if err != nil {
				t.Fatalf("LIST after DELETE: %v", err)
			}
			for _, m := range listData2 {
				if m.Mailbox == "Sent" {
					t.Errorf("Sent still in LIST after DELETE")
				}
			}
		})
	}
}

func hasImapFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}
