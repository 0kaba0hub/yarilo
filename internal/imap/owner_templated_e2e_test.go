package imap_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// ownerTemplatedServer brings up a server with an owner-templated namespace
// (user/%u/) and a userdb lookup that resolves each owner to their own maildir
// store -- the B1 configuration, reachable at last. The owner's driver comes
// from the userdb (maildir here), the root from the namespace template expanded
// against the owner.
func ownerTemplatedServer(t *testing.T) (root string, dial func(user string) *imapclient.Client) {
	t.Helper()
	root = t.TempDir()

	// Every owner's store is <root>/<owner>/Maildir; the userdb hands back that
	// home with the maildir driver, and the namespace template maildir:%h places
	// the store there.
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner != "alice" && owner != "bob" {
			return nil, &notFoundError{owner}
		}
		home := filepath.Join(root, owner)
		return &mailboxpkg.UserInfo{
			Username: owner, Home: home, MailPath: home, Driver: "maildir",
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
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: true, Location: "maildir:%h/Maildir"},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	dial = func(user string) *imapclient.Client {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		c := imapclient.New(conn, nil)
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if err := c.Login(user, "pw").Wait(); err != nil {
			t.Fatalf("login %q: %v", user, err)
		}
		t.Cleanup(func() { c.Logout().Wait() }) //nolint:errcheck
		return c
	}
	return root, dial
}

type notFoundError struct{ who string }

func (e *notFoundError) Error() string { return "userdb: no such user " + e.who }

// The owner owns their own templated namespace: alice creates, selects and
// administers user/alice/* with no ACL grant, because isOwner is true for her
// there -- the whole point of B1.
func TestOwnerTemplated_OwnerOwnsTheirOwnSpace(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	a := dial("alice")

	if err := a.Create("user/alice/Reports", nil).Wait(); err != nil {
		t.Fatalf("owner CREATE in her own templated namespace: %v", err)
	}
	if _, err := a.Select("user/alice/Reports", nil).Wait(); err != nil {
		t.Errorf("owner SELECT of her own folder: %v", err)
	}
	// GETACL shows the owner with full rights (the implicit owner entry, now
	// keyed on the resolved owner, not the caller).
	data, err := a.GetACL("user/alice/Reports").Wait()
	if err != nil {
		t.Fatalf("owner GETACL: %v", err)
	}
	alice, _ := imaplib.NewRightsIdentifierUsername("alice")
	if got := string(data.Rights[alice]); got != string(mailboxpkg.FullRights) {
		t.Errorf("owner implicit rights = %q, want full", got)
	}
}

// A peer is not the owner of someone else's templated namespace: bob reaching
// user/alice/* is a non-owner and, with no grant, gets the absent-mailbox
// answer -- isOwner is false for him there (person-based, #1130).
func TestOwnerTemplated_PeerIsNotTheOwner(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	a := dial("alice")
	if err := a.Create("user/alice/Reports", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}

	b := dial("bob")
	_, err := b.Select("user/alice/Reports", nil).Wait()
	if err == nil {
		t.Fatal("peer SELECT of another owner's folder without a grant should fail")
	}
}

// An owner the userdb does not know is NO [NONEXISTENT], not a usable namespace
// and not the caller's own store (§7.6).
func TestOwnerTemplated_UnknownOwnerIsNonexistent(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	a := dial("alice")
	_, err := a.Select("user/ghost/X", nil).Wait()
	if err == nil {
		t.Fatal("SELECT under an unknown owner should fail")
	}
	if code := aclErrCode(err); code != imaplib.ResponseCodeNonExistent {
		t.Errorf("unknown owner: code %q, want NONEXISTENT", code)
	}
}

// A malformed owner segment resolves to nobody, never the caller: user/../x
// makes extractOwner return not-ok, so it falls through and is not treated as
// the session user's own space (#544/B1, the honesty of isOwner).
func TestOwnerTemplated_MalformedOwnerIsNobody(t *testing.T) {
	_, dial := ownerTemplatedServer(t)
	a := dial("alice")
	// Should not resolve to alice's own store; it is not a valid owner.
	if _, err := a.Select("user/../Reports", nil).Wait(); err == nil {
		t.Error("a traversal owner segment resolved to a selectable mailbox")
	}
}

// Two owners get two stores: alice's folder is invisible to bob's namespace and
// vice versa -- the template distinguishes owners.
func TestOwnerTemplated_TwoOwnersTwoStores(t *testing.T) {
	root, dial := ownerTemplatedServer(t)
	a := dial("alice")
	if err := a.Create("user/alice/Only", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}
	if _, err := a.Select("user/alice/Only", nil).Wait(); err != nil {
		t.Errorf("alice cannot see her own folder: %v", err)
	}
	// The maildir landed under alice's home (<root>/alice/Maildir), proving the
	// root came from the namespace template expanded against the owner -- not a
	// shared path, and not bob's.
	aliceFolder := filepath.Join(root, "alice", "Maildir", ".Only")
	if _, err := os.Stat(aliceFolder); err != nil {
		t.Errorf("alice's folder not under her own store: %v", err)
	}
	bobPath := filepath.Join(root, "bob", "Maildir", ".Only")
	if _, err := os.Stat(bobPath); err == nil {
		t.Errorf("alice's folder leaked into bob's store at %s", bobPath)
	}
}

// A declared owner-templated namespace with no userdb lookup wired is a config
// error caught at startup, not one SELECT at a time -- the #1132 fail-loud rule
// at the server layer.
func TestOwnerTemplated_NoUserdbLookupFailsStartup(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New accepted a templated namespace with no userdb lookup; want a startup panic")
		}
		if !strings.Contains(strings.ToLower(toStr(r)), "userdb") {
			t.Errorf("panic %v does not name the missing userdb lookup", r)
		}
	}()
	imapserver.New(imapserver.Options{
		Mailbox: maildir.New(),
		Index:   file.New(),
		// UserdbLookup deliberately nil.
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', Location: "maildir:%h"},
		},
	})
}

func toStr(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
