package imap_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// The sandbox shape, which the Shared/ harness does not have: a public
// namespace whose location belongs to nobody -- not anchored in any user's
// home. #1317 was found there.
func publicNSServer(t *testing.T) (publicRoot string, dial func(user string) *imapclient.Client) {
	t.Helper()
	root := t.TempDir()
	publicRoot = filepath.Join(root, "vhosts", "public")
	if err := os.MkdirAll(publicRoot, 0o700); err != nil {
		t.Fatalf("mkdir public: %v", err)
	}
	passdb := &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}}
	srv := imapserver.New(imapserver.Options{
		Mailbox:    maildir.New(),
		Index:      file.New(),
		Resolver:   &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:       passdb,
		ACLEnabled: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "Public/", Separator: '/', Location: "maildir:" + publicRoot, List: imapserver.ListYes},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go srv.Serve(ln)                 //nolint:errcheck

	dial = func(user string) *imapclient.Client {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() }) //nolint:errcheck
		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("login %s: %v", user, err)
		}
		t.Cleanup(func() { c.Close() }) //nolint:errcheck
		return c
	}
	return publicRoot, dial
}

func TestPublicNamespacePeerCannotSelect(t *testing.T) {
	publicRoot, dial := publicNSServer(t)

	// The sandbox grant: alice may create under the public root.
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lk\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}

	a := dial("alice")
	if err := a.Create("Public/ProbeQA", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}

	b := dial("bob")
	_, selErr := b.Select("Public/ProbeQA", nil).Wait()
	if selErr == nil {
		t.Error("a peer with no rights SELECTed a public mailbox")
	}
	_, absentErr := b.Select("Public/AbsentQA", nil).Wait()
	if absentErr == nil {
		t.Fatal("SELECT of an absent name succeeded")
	}
	if selErr != nil && aclErrCode(selErr) != aclErrCode(absentErr) {
		t.Errorf("existing answers %q, absent %q -- the difference enumerates names",
			aclErrCode(selErr), aclErrCode(absentErr))
	}
	if code := aclErrCode(absentErr); code != imaplib.ResponseCodeNonExistent {
		t.Errorf("absent mailbox answered %q, want NONEXISTENT", code)
	}
}
