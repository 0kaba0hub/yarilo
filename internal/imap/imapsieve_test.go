package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/sieve"
	file "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/memory"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// imapSieveScript handles both causes so one bound script serves every test.
const imapSieveScript = `require ["imapsieve", "environment", "fileinto", "mailbox"];
if environment :is "imap.cause" "COPY" { fileinto :create "Quarantine"; stop; }
if environment :is "imap.cause" "APPEND" { fileinto :create "Archive"; }
`

// startImapSieveClient brings up an IMAP server with imapsieve enabled and a
// single admin script ("act") available for binding via METADATA.
func startImapSieveClient(t *testing.T) *imapclient.Client {
	t.Helper()
	scriptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptDir, "act.sieve"), []byte(imapSieveScript), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := sieve.New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxActions: 32, MaxScriptSize: 65536,
		DefaultName: "yarilo", ImapSieveEnabled: true, ImapSieveScriptDir: scriptDir,
	}, nil, nil, nil)

	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })

	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		Auth:         &stubPassdb{user: "user@test.com", pass: "testpass"},
		MetadataDict: md,
		SieveEngine:  eng,
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
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

func bindImapSieve(t *testing.T, c *imapclient.Client, mbox string) {
	t.Helper()
	val := []byte("act")
	if err := c.SetMetadata(mbox, map[string]*[]byte{"/shared/imapsieve/script": &val}).Wait(); err != nil {
		t.Fatalf("SETMETADATA %q: %v", mbox, err)
	}
}

func appendTo(t *testing.T, c *imapclient.Client, mbox string) {
	t.Helper()
	body := []byte("Subject: hi\r\n\r\nbody\r\n")
	ac := c.Append(mbox, int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("APPEND %q: %v", mbox, err)
	}
}

func numMessages(t *testing.T, c *imapclient.Client, mbox string) uint32 {
	t.Helper()
	sel, err := c.Select(mbox, nil).Wait()
	if err != nil {
		t.Fatalf("SELECT %q: %v", mbox, err)
	}
	n := sel.NumMessages
	if err := c.Unselect().Wait(); err != nil {
		t.Fatalf("UNSELECT: %v", err)
	}
	return n
}

// TestImapSieveAppendRefiles: a script bound to INBOX refiles an APPENDed
// message into Archive (RFC 6785 APPEND cause).
func TestImapSieveAppendRefiles(t *testing.T) {
	c := startImapSieveClient(t)
	bindImapSieve(t, c, "INBOX")
	appendTo(t, c, "INBOX")

	if n := numMessages(t, c, "INBOX"); n != 0 {
		t.Errorf("INBOX has %d messages, want 0 (refiled away)", n)
	}
	if n := numMessages(t, c, "Archive"); n != 1 {
		t.Errorf("Archive has %d messages, want 1", n)
	}
}

// TestImapSieveCopyRefiles: a script bound to the COPY destination refiles the
// copy into Quarantine (RFC 6785 COPY cause); the source is untouched.
func TestImapSieveCopyRefiles(t *testing.T) {
	c := startImapSieveClient(t)
	if err := c.Create("Target", nil).Wait(); err != nil {
		t.Fatalf("CREATE Target: %v", err)
	}
	bindImapSieve(t, c, "Target")

	// INBOX has no binding, so the APPEND stays there.
	appendTo(t, c, "INBOX")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}
	if _, err := c.Copy(imap.SeqSetNum(1), "Target").Wait(); err != nil {
		t.Fatalf("COPY: %v", err)
	}
	if err := c.Unselect().Wait(); err != nil {
		t.Fatalf("UNSELECT: %v", err)
	}

	if n := numMessages(t, c, "INBOX"); n != 1 {
		t.Errorf("INBOX has %d messages, want 1 (COPY leaves source)", n)
	}
	if n := numMessages(t, c, "Target"); n != 0 {
		t.Errorf("Target has %d messages, want 0 (copy refiled away)", n)
	}
	if n := numMessages(t, c, "Quarantine"); n != 1 {
		t.Errorf("Quarantine has %d messages, want 1", n)
	}
}
