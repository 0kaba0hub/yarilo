package imap_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// rawSharedServer brings up the shared-namespace server and returns its raw
// address, so a test can speak IMAP by hand. imapclient always uses LITERAL+,
// which never reproduced the synchronizing-literal hang (#1126).
func rawSharedServer(t *testing.T) (aliceHome, addr string) {
	t.Helper()
	root := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      file.New(),
		Resolver:   &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:       &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}},
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "Shared/", Separator: '/', Location: "maildir:" + filepath.Join(root, "alice", "Maildir"), List: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return filepath.Join(root, "alice"), ln.Addr().String()
}

// APPEND denied by ACL must still return a tagged response, even with a
// synchronizing literal (RFC 9051 §6.3.12). It did not: the rights check
// returned early without reading the literal, the tagged NO was lost, and a
// client that sent the message saw it neither stored nor refused -- the
// connection stayed usable (a following NOOP answered) but the command hung
// (#1126).
func TestAppendDenialAnswersWithSynchronizingLiteral(t *testing.T) {
	aliceHome, addr := rawSharedServer(t)

	a := dialRaw(t, addr)
	if !strings.Contains(a.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("alice login")
	}
	seedACL(t, aliceHome, "INBOX", "user=bob lr\n") // read, not insert

	b := dialRaw(t, addr)
	if !strings.Contains(b.cmd("LOGIN bob pw"), "OK") {
		t.Fatal("bob login")
	}

	body := "From: x@y\r\nSubject: t\r\n\r\nhi\r\n"
	fmt.Fprintf(b.conn, "b2 APPEND Shared/INBOX {%d}\r\n", len(body))
	if cont := b.readLine(); !strings.HasPrefix(cont, "+") {
		t.Fatalf("expected continuation request, got %q", cont)
	}
	// A malformed command tail -- an extra CR, exactly what openssl s_client
	// -crlf produced from a client's explicit \r\n. ExpectCRLF fails on it, and
	// the library used to return a nil error there, swallowing the tagged
	// response for a command that had already run. The client is owed a BAD,
	// not silence (RFC 9051 §6.3.12).
	fmt.Fprintf(b.conn, "%s\r\r\n", body)

	// The tagged response for b2 must arrive within a bound. Read lines until
	// the tag or a read timeout; a timeout is the hang.
	b.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got string
	for {
		line, err := b.r.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "b2 ") {
			got = strings.TrimRight(line, "\r\n")
			break
		}
	}
	if got == "" {
		t.Fatal("APPEND denial returned no tagged response (#1126): the client cannot tell refused from lost")
	}
	// A tagged response of any kind satisfies the RFC. The framing was broken,
	// so BAD is the honest answer; NO would mean the library reached the ACL
	// verdict first. Either is a response; silence is the defect.
	if !strings.Contains(got, "BAD") && !strings.Contains(got, "NO") {
		t.Errorf("APPEND answered %q, want a tagged BAD or NO", got)
	}
}

// An APPEND the server accepts and stores must answer OK, even when the command
// tail is malformed. #1127 gave the malformed tail a tagged response, but it was
// BAD -- and the store runs before the trailing CRLF is read, so an accepted
// APPEND with a mis-framed tail stored the message and then reported BAD. A
// client reads BAD (RFC 9051 §7.1.3: the command did not run), retries, and
// stores a second copy. The truthful answer to a store that happened is OK
// (#1129).
func TestAppendStoredAnswersOKOnMalformedTail(t *testing.T) {
	aliceHome, addr := rawSharedServer(t)

	a := dialRaw(t, addr)
	if !strings.Contains(a.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("alice login")
	}

	body := "From: x@y\r\nSubject: t\r\n\r\nhi\r\n"
	fmt.Fprintf(a.conn, "a2 APPEND INBOX {%d}\r\n", len(body))
	if cont := a.readLine(); !strings.HasPrefix(cont, "+") {
		t.Fatalf("expected continuation request, got %q", cont)
	}
	// The same malformed tail as the denial test -- an extra CR -- but on an
	// APPEND alice is allowed to make into her own INBOX.
	fmt.Fprintf(a.conn, "%s\r\r\n", body)

	a.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got string
	for {
		line, err := a.r.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "a2 ") {
			got = strings.TrimRight(line, "\r\n")
			break
		}
	}
	if got == "" {
		t.Fatal("APPEND returned no tagged response")
	}
	if !strings.Contains(got, "OK") {
		t.Errorf("stored APPEND with a malformed tail answered %q, want OK (#1129)", got)
	}

	// Exactly one message on disk: the store must have happened once, not zero
	// times (silently dropped) and not twice (a retry the OK now prevents).
	cur := filepath.Join(aliceHome, "Maildir", "cur")
	newDir := filepath.Join(aliceHome, "Maildir", "new")
	n := countFiles(t, cur) + countFiles(t, newDir)
	if n != 1 {
		t.Errorf("message count on disk = %d, want exactly 1", n)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("readdir %s: %v", dir, err)
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			n++
		}
	}
	return n
}
