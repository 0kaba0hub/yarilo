package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

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

// TestImapSieveAppendRefiles is the end-to-end guard for the imapsieve APPEND
// hook (RFC 6785): a script bound to INBOX via /shared/imapsieve/script refiles
// an APPENDed message into Archive, so INBOX ends empty and Archive holds it.
func TestImapSieveAppendRefiles(t *testing.T) {
	scriptDir := t.TempDir()
	script := `require ["imapsieve", "environment", "fileinto", "mailbox"];` + "\n" +
		`if environment :is "imap.cause" "APPEND" { fileinto :create "Archive"; }` + "\n"
	if err := os.WriteFile(filepath.Join(scriptDir, "refile.sieve"), []byte(script), 0o600); err != nil {
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

	// Bind the script to INBOX via the imapsieve METADATA annotation.
	val := []byte("refile")
	if err := c.SetMetadata("INBOX", map[string]*[]byte{"/shared/imapsieve/script": &val}).Wait(); err != nil {
		t.Fatalf("SETMETADATA: %v", err)
	}

	// APPEND to INBOX — the hook should refile it into Archive.
	body := []byte("Subject: hi\r\n\r\nbody\r\n")
	ac := c.Append("INBOX", int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("APPEND: %v", err)
	}

	inbox, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}
	if inbox.NumMessages != 0 {
		t.Errorf("INBOX has %d messages, want 0 (refiled away by imapsieve)", inbox.NumMessages)
	}
	if err := c.Unselect().Wait(); err != nil {
		t.Fatalf("UNSELECT: %v", err)
	}
	arch, err := c.Select("Archive", nil).Wait()
	if err != nil {
		t.Fatalf("SELECT Archive: %v", err)
	}
	if arch.NumMessages != 1 {
		t.Errorf("Archive has %d messages, want 1", arch.NumMessages)
	}
}
