package imap_test

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var statusAll = imap.StatusOptions{NumMessages: true, UIDValidity: true, UIDNext: true}

func appendN(t *testing.T, c *imapclient.Client, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("From: a@b.com\r\nSubject: m%d\r\n\r\nbody\r\n", i)
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
}

// STATUS must refuse a name that resolves outside the mailbox instead of
// answering for it. The answer is not merely wrong: OpenFolder creates the
// index it is asked for, so STATUS on such a name initialised a fresh index at
// that path and reported an empty mailbox to the client (#1072).
func TestStatusRefusesNamesOutsideTheMailbox(t *testing.T) {
	for _, bf := range backends {
		t.Run(bf.name, func(t *testing.T) {
			c := startServerWith(t, bf.new(t))
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendN(t, c, 8)

			for _, name := range []string{"..", ".", "", "../victim@x/Maildir", "./../victim@x/Maildir"} {
				sd, err := c.Status(name, &statusAll).Wait()
				if err == nil {
					n := uint32(0)
					if sd.NumMessages != nil {
						n = *sd.NumMessages
					}
					t.Errorf("STATUS %q answered MESSAGES=%d UIDVALIDITY=%d; the name resolves outside the mailbox",
						name, n, sd.UIDValidity)
					continue
				}
				var ie *imap.Error
				if !errors.As(err, &ie) {
					t.Errorf("STATUS %q returned %T (%v), want an IMAP error", name, err, err)
					continue
				}
				if ie.Code != imap.ResponseCodeNonExistent {
					t.Errorf("STATUS %q answered code %q, want NONEXISTENT — the same answer SELECT and DELETE give for these names",
						name, ie.Code)
				}
			}
		})
	}
}

// STATUS must still answer for a mailbox that exists — otherwise the test
// above passes on a server that refuses every STATUS.
func TestStatusStillAnswersForRealMailboxes(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	appendN(t, c, 3)

	sd, err := c.Status("INBOX", &statusAll).Wait()
	if err != nil {
		t.Fatalf("STATUS INBOX: %v", err)
	}
	if sd.NumMessages == nil || *sd.NumMessages != 3 {
		t.Errorf("STATUS INBOX reported %v messages, want 3", sd.NumMessages)
	}
}

// A refused name is a client error, not a server fault. NO [SERVERBUG] tells
// the operator to go looking for a crash that never happened (#1072).
func TestCreateWithInvalidNameIsNotReportedAsAServerBug(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	err := c.Create("../escaped", nil).Wait()
	if err == nil {
		t.Fatal("CREATE \"../escaped\" was accepted")
	}
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Fatalf("CREATE returned %T (%v), want an IMAP error", err, err)
	}
	if ie.Code == "SERVERBUG" {
		t.Errorf("CREATE answered SERVERBUG for an invalid name: %q", ie.Text)
	}
	if ie.Code != imap.ResponseCodeCannot {
		t.Errorf("CREATE answered code %q, want CANNOT", ie.Code)
	}
}

// APPEND, COPY and MOVE opened the folder handle before checking that the
// mailbox exists, and opening initialises the index -- so a refused name still
// left index state behind, which is the same defect STATUS had (#1072).
//
// The response code is asserted per command: TRYCREATE is what tells a client
// to create the mailbox and retry, and SERVERBUG would send the operator
// hunting a crash that never happened.
func TestAppendCopyMoveRefuseNamesOutsideTheMailbox(t *testing.T) {
	for _, name := range []string{"..", ".", "", "../victim@x/Maildir"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			c := startServerWithRoot(t, maildirBackend(t), root)
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			appendN(t, c, 1)
			// The baseline is taken after SELECT, which legitimately creates
			// the subscriptions file -- otherwise the test reports the
			// server's own bookkeeping as a defect.
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			before := indexPaths(t, root)

			const body = "From: a@b.com\r\nSubject: x\r\n\r\nx\r\n"
			ac := c.Append(name, int64(len(body)), nil)
			_, _ = ac.Write([]byte(body))
			_ = ac.Close()
			_, err := ac.Wait()
			assertTryCreate(t, "APPEND", err)

			_, err = c.Copy(seqSetAll(), name).Wait()
			assertTryCreate(t, "COPY", err)

			// MOVE kept a FolderExists block of its own that the sentinel made
			// unreachable, so the case looked handled while the bare error was
			// still reported as SERVERBUG.
			_, err = c.Move(seqSetAll(), name).Wait()
			assertTryCreate(t, "MOVE", err)

			after := indexPaths(t, root)
			for _, p := range after {
				found := false
				for _, q := range before {
					if p == q {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("index state created for a mailbox that does not exist: %s", p)
				}
			}
		})
	}
}

func assertTryCreate(t *testing.T, cmd string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s on a name outside the mailbox was accepted", cmd)
	}
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Fatalf("%s returned %T (%v), want an IMAP error", cmd, err, err)
	}
	if ie.Code == "SERVERBUG" {
		t.Errorf("%s answered SERVERBUG for a client naming mistake: %q", cmd, ie.Text)
	}
	if ie.Code != imap.ResponseCodeTryCreate {
		t.Errorf("%s answered code %q, want TRYCREATE", cmd, ie.Code)
	}
}

// startServerWithRoot mirrors startServerWith but hands back the storage root
// so a test can look at what landed on disk.
func startServerWithRoot(t *testing.T, mb mailbox.MailboxBackend, root string) *imapclient.Client {
	return startServerWithOptsBackend(t, root, mb, nil)
}

// startServerWithOpts is startServerWithRoot with a metadata dict wired up.
func startServerWithOpts(t *testing.T, root string, md dict.Dict) *imapclient.Client {
	return startServerWithOptsBackend(t, root, maildirBackend(t), md)
}

func startServerWithOptsBackend(t *testing.T, root string, mb mailbox.MailboxBackend, md dict.Dict) *imapclient.Client {
	t.Helper()
	opts := imapserver.Options{
		Mailbox:      mb,
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Auth:         &stubPassdb{user: "user@test.com", pass: "testpass"},
		MetadataDict: md,
	}
	srv := imapserver.New(opts)
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
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

// indexDirCount counts every path under root: index state created for a
// mailbox that does not exist shows up as new entries.
func indexPaths(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	return out
}

func seqSetAll() imap.NumSet {
	var ss imap.SeqSet
	ss.AddRange(1, 0)
	return ss
}

// METADATA resolves the mailbox through metadataResolve, the last OpenFolder
// outside ensureFolderHandle. It answered OK for a name that resolves outside
// the mailbox and left a complete index behind for it (#1072).
func TestMetadataRefusesNamesOutsideTheMailbox(t *testing.T) {
	root := t.TempDir()
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })

	c := startServerWithOpts(t, root, md)
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	appendN(t, c, 1)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := indexPaths(t, root)

	for _, name := range []string{"..", ".", "../victim@x/Maildir"} {
		_, err := c.GetMetadata(name, []string{"/private/comment"}, &imap.GetMetadataOptions{}).Wait()
		if err == nil {
			t.Errorf("GETMETADATA %q was answered for a name outside the mailbox", name)
			continue
		}
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Code != imap.ResponseCodeNonExistent {
			t.Errorf("GETMETADATA %q answered code %q, want NONEXISTENT", name, ie.Code)
		}
	}

	for _, p := range indexPaths(t, root) {
		found := false
		for _, q := range before {
			if p == q {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("index state created for a mailbox that does not exist: %s", p)
		}
	}
}

// METADATA must still answer for a real mailbox.
func TestMetadataStillAnswersForRealMailboxes(t *testing.T) {
	md, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })

	c := startServerWithOpts(t, t.TempDir(), md)
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	if _, err := c.GetMetadata("INBOX", []string{"/private/comment"}, &imap.GetMetadataOptions{}).Wait(); err != nil {
		t.Errorf("GETMETADATA INBOX: %v", err)
	}
}
