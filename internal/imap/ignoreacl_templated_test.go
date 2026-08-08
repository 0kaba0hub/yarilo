package imap_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// The 7.9 edge, pinned: acl_ignore on an owner-templated namespace. The
// reference clears ownership for every non-private namespace in the same code
// its acl flags drive; ours resolves the owner and gives them the owner tier,
// so the combination had no recorded answer. The answer: acl_ignore means
// what it says -- rights are not checked for anyone, owner or peer, and an
// on-disk entry (a negative one included) is inert. The owner tier changes
// nothing here because there is nothing left for it to short-circuit; it
// matters only where ACL is enforced, and there it is covered by the
// owner-templated e2e tests.
func startIgnoreACLServer(t *testing.T) (root, addr string) {
	t.Helper()
	root = t.TempDir()
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner != "alice" && owner != "bob" {
			return nil, &notFoundError{owner}
		}
		home := filepath.Join(root, owner)
		return &mailboxpkg.UserInfo{
			Username: owner, Home: home, MailPath: filepath.Join(home, "Maildir"), Driver: "maildir",
		}, nil
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:         &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}},
		ACLEnabled:   true,
		UserdbLookup: lookup,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: imapserver.ListChildren,
				Location: "maildir:%h/Maildir", IgnoreACL: true},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return root, ln.Addr().String()
}

func TestIgnoreACL_OwnerTierChangesNothing(t *testing.T) {
	root, addr := startIgnoreACLServer(t)

	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)

	// The owner works in their own space -- true with or without the tier.
	if out := a.cmd(`SELECT user/alice/INBOX`); !strings.Contains(out, "OK") {
		t.Fatalf("owner cannot select their own templated INBOX: %s", out)
	}

	// A denying entry on disk, negative included, is inert under acl_ignore:
	// bob has no grant and an explicit -user=bob against him, and still gets in.
	aclFile := filepath.Join(root, "alice", "Maildir", "yarilo-acl")
	rows := "-user=bob lrswipkxtea\n"
	if err := os.WriteFile(aclFile, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	b := orphanLogin(t, addr, "bob")
	if out := b.cmd(`SELECT user/alice/INBOX`); !strings.Contains(out, "OK") {
		t.Errorf("acl_ignore namespace still checked rights for a peer: %s", out)
	}
	// And LIST materialisation is not gated either: the peer sees the space.
	if out := b.cmd(`LIST "" "user/alice/*"`); !strings.Contains(out, `"user/alice/INBOX"`) {
		t.Errorf("acl_ignore namespace hid the owner's folders from a peer:\n%s", out)
	}
}

// With ACL enforced on the same shape, the same denying entry works and the
// owner tier holds -- the distinguishing pair for the case above: remove
// IgnoreACL and the peer's answers flip while the owner's do not.
func TestIgnoreACL_ContrastWithEnforced(t *testing.T) {
	root, dial := ownerTemplatedServer(t)
	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	aclFile := filepath.Join(root, "alice", "Maildir", "yarilo-acl")
	rows := "-user=bob lrswipkxtea\n"
	if err := os.WriteFile(aclFile, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	b := dial("bob")
	if _, err := b.Select("user/alice/INBOX", nil).Wait(); err == nil {
		t.Error("enforced namespace let a denied peer in")
	}
	// The owner is untouched by the denying file -- the strong grant (7.6).
	if _, err := a.Select("user/alice/INBOX", nil).Wait(); err != nil {
		t.Errorf("owner blocked in their own space under enforcement: %v", err)
	}
}
