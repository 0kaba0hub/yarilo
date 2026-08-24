package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadServer seeds a mailbox with real messages and serves it, because the
// claim under test is the whole path: headers on disk, criteria, algorithm,
// and the parentheses on the wire.
func threadServer(t *testing.T, raws []string) (net.Conn, *bufio.Reader) {
	conn, rd, _ := threadServerIn(t, raws)
	return conn, rd
}

// threadServerIn also hands back the storage root, so a row can break the
// stored messages under the running session.
func threadServerIn(t *testing.T, raws []string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo("user@test.com", "")

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := fileindex.New().OpenUser(info)
	for i, raw := range raws {
		uid := uint32(i + 1)
		name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), uid, int64(len(raw)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		f, err := ui.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatalf("open folder: %v", err)
		}
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(raw)), VSize: vsize,
			GUID: guid, InternalDate: time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	srv := imapserver.New(imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    fileindex.New(),
		Resolver: resolver,
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
	})
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
	rd := bufio.NewReader(conn)
	readUntilTag(t, rd, "")
	command(t, conn, rd, "a1", "LOGIN user@test.com testpass")
	command(t, conn, rd, "a2", "SELECT INBOX")
	return conn, rd, root
}

func threadLine(t *testing.T, conn net.Conn, rd *bufio.Reader, cmd string) string {
	t.Helper()
	for _, line := range command(t, conn, rd, "a3", cmd) {
		if strings.HasPrefix(line, "* THREAD") {
			return line
		}
	}
	return ""
}

func mailOf(id, subject, date string, refs string) string {
	msg := fmt.Sprintf("Message-ID: <%s>\r\nSubject: %s\r\nDate: %s\r\n", id, subject, date)
	if refs != "" {
		msg += "References: " + refs + "\r\n"
	}
	return msg + "\r\nbody\r\n"
}

// The conversation on disk comes back as the tree RFC 5256 describes, over the
// wire, with the parentheses that carry it.
func TestThreadAnswersTheConversation(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
		mailOf("c@x", "Re: Plan", "Sun, 1 Mar 2026 12:00:00 +0000", "<a@x>"),
		mailOf("d@x", "Unrelated", "Sun, 1 Mar 2026 13:00:00 +0000", ""),
	}
	conn, rd := threadServer(t, raws)

	if got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL"); got != "* THREAD (1 (2)(3))(4)" {
		t.Errorf("REFERENCES = %q, want the branch under message 1", got)
	}
}

// The two algorithms are asked the same question about the same mailbox and
// must answer differently, because they read different evidence: a reply that
// changed the subject is a reply by ancestry and a stranger by subject.
//
// Seeded so that agreement is impossible -- with replies that keep the subject
// both algorithms produce one tree, and a row like that would pass with either
// implementation wired to both names.
func TestTheTwoAlgorithmsReadDifferentEvidence(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Budget for it", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
		mailOf("c@x", "Re: Plan", "Sun, 1 Mar 2026 12:00:00 +0000", ""),
	}
	conn, rd := threadServer(t, raws)

	// Ancestry: 2 answers 1, and 3 joins by subject (step 5).
	if got := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL"); got != "* THREAD (1 (2)(3))" {
		t.Errorf("REFERENCES = %q, want the References chain and the subject join", got)
	}
	// Subjects alone: 2 is its own thread, 3 belongs with 1.
	for _, line := range command(t, conn, rd, "a4", "THREAD ORDEREDSUBJECT UTF-8 ALL") {
		if strings.HasPrefix(line, "* THREAD") && line != "* THREAD (1 3)(2)" {
			t.Errorf("ORDEREDSUBJECT = %q, want subjects only", line)
		}
	}
}

// The search criteria select which messages are threaded, so a reply excluded
// by the search cannot appear in the tree.
func TestThreadThreadsOnlyTheSearchedMessages(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
		mailOf("c@x", "Budget", "Sun, 1 Mar 2026 12:00:00 +0000", ""),
	}
	conn, rd := threadServer(t, raws)

	if got := threadLine(t, conn, rd, `THREAD REFERENCES UTF-8 SUBJECT "Budget"`); got != "* THREAD (3)" {
		t.Errorf("filtered THREAD = %q, want only the searched message", got)
	}
}

// UID THREAD numbers the same tree by UID. With sequence numbers and UIDs
// equal the two commands are indistinguishable, so the mailbox is seeded to
// make them differ.
func TestUIDThreadAnswersUIDs(t *testing.T) {
	raws := []string{
		mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", ""),
		mailOf("b@x", "Re: Plan", "Sun, 1 Mar 2026 11:00:00 +0000", "<a@x>"),
	}
	conn, rd := threadServer(t, raws)
	// Expunging the first message moves every remaining sequence number one
	// below its UID.
	command(t, conn, rd, "x1", "STORE 1 +FLAGS (\\Deleted)")
	command(t, conn, rd, "x2", "EXPUNGE")

	seq := threadLine(t, conn, rd, "THREAD REFERENCES UTF-8 ALL")
	if seq != "* THREAD (1)" {
		t.Fatalf("THREAD = %q, want sequence number 1", seq)
	}
	conn2, rd2 := conn, rd
	for _, line := range command(t, conn2, rd2, "a4", "UID THREAD REFERENCES UTF-8 ALL") {
		if strings.HasPrefix(line, "* THREAD") && line != "* THREAD (2)" {
			t.Errorf("UID THREAD = %q, want the UID 2", line)
		}
	}
}

// A mailbox where nothing matches answers with the keyword and no threads --
// not an error, and not silence.
func TestThreadWithNoMatchesIsAnEmptyReply(t *testing.T) {
	conn, rd := threadServer(t, []string{mailOf("a@x", "Plan", "Sun, 1 Mar 2026 10:00:00 +0000", "")})

	if got := threadLine(t, conn, rd, `THREAD REFERENCES UTF-8 SUBJECT "nothing"`); got != "* THREAD" {
		t.Errorf("empty THREAD = %q, want the bare keyword", got)
	}
}
