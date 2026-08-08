package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func startEnvelopeCacheServer(t *testing.T) (root, addr string) {
	t.Helper()
	root = t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	return root, ln.Addr().String()
}

func appendEnvelopeMsg(t *testing.T, rc *rawConn) {
	t.Helper()
	rc.seq++
	body := "From: Alice Liddell <alice@example.com>\r\n" +
		"To: bob@example.com\r\n" +
		"Subject: Тема з UTF-8\r\n" +
		"Message-ID: <m1@example.com>\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n\r\nhi\r\n"
	tag := "e001"
	rc.conn.Write([]byte(tag + " APPEND INBOX {" + itoa(len(body)) + "}\r\n"))
	rc.readLine() // continuation
	rc.conn.Write([]byte(body + "\r\n"))
	for {
		if strings.HasPrefix(rc.readLine(), tag+" ") {
			return
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// The claim of #1030: FETCH ENVELOPE never opens the message once cached.
// Proven the only honest way -- the message file is REMOVED between the two
// fetches, so the second answer can come from nowhere else.
func TestFetchEnvelope_ServedFromCacheWithoutTheMessage(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)

	first := c.cmd(`FETCH 1 (ENVELOPE)`)
	if !strings.Contains(first, "alice") || !strings.Contains(first, "example.com") {
		t.Fatalf("first fetch did not parse the envelope:\n%s", first)
	}

	// Remove the message from disk: an uncached second fetch has nothing to
	// parse and would answer without an envelope.
	var msgFile string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() &&
			(strings.Contains(p, "/cur/") || strings.Contains(p, "/new/")) {
			msgFile = p
		}
		return nil
	})
	if msgFile == "" {
		t.Fatal("message file not found on disk")
	}
	if err := os.Remove(msgFile); err != nil {
		t.Fatal(err)
	}

	second := c.cmd(`FETCH 1 (ENVELOPE)`)
	if !strings.Contains(second, "alice") || !strings.Contains(second, "=?utf-8?") && !strings.Contains(second, "Тема") {
		t.Fatalf("second fetch not served from the cache:\n%s", second)
	}
}

// A corrupt cache file is the first kind of "not there": the fetch parses as
// today, the file is replaced, and the client sees nothing.
func TestFetchEnvelope_CorruptCacheDegradesToParse(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)
	c.cmd(`FETCH 1 (ENVELOPE)`) // populate

	var cachePath string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && filepath.Base(p) == "yarilo.index.cache" {
			cachePath = p
		}
		return nil
	})
	if cachePath == "" {
		t.Fatal("cache file was not created by the first fetch")
	}
	if err := os.WriteFile(cachePath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := c.cmd(`FETCH 1 (ENVELOPE)`)
	if !strings.Contains(out, "alice") {
		t.Fatalf("corrupt cache leaked to the client:\n%s", out)
	}
	if strings.Contains(out, "NO ") || strings.Contains(out, "BAD ") {
		t.Fatalf("corrupt cache became a client error:\n%s", out)
	}
}
