package imap_test

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// startRegistryServer is startOrphanServer plus the owner-discovery dict.
func startRegistryServer(t *testing.T) (d dict.Dict, addr string) {
	t.Helper()
	root := t.TempDir()
	var err error
	d, err = dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// The owner's store IS the personal store (MailPath == the resolver's
	// home), which is the deployed shape: both come from one userdb. That
	// coincidence is what routes personal-namespace grants into discovery.
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner != "alice" && owner != "bob" && owner != "carol" {
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
		Auth:         &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw", "carol": "pw"}},
		ACLEnabled:   true,
		UserdbLookup: lookup,
		SharedDict:   d,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: imapserver.ListChildren,
				Location: "maildir:%h"},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return d, ln.Addr().String()
}

// The function #1168 exists for: a grant makes the owner's space appear in the
// grantee's LIST user/* -- discovery without guessing the owner's name -- and
// a revocation makes it disappear again.
func TestOwnerRegistry_GrantMakesOwnerDiscoverable(t *testing.T) {
	_, addr := startRegistryServer(t)

	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`SETACL user/alice/INBOX bob lr`), "OK") {
		t.Fatal("grant failed")
	}

	b := orphanLogin(t, addr, "bob")
	out := b.cmd(`LIST "" "user/*"`)
	if !strings.Contains(out, `"user/alice/INBOX"`) {
		t.Fatalf("granted peer's user/* does not discover the owner:\n%s", out)
	}

	// Nothing was granted to carol: her user/* stays empty.
	c := orphanLogin(t, addr, "carol")
	if out := c.cmd(`LIST "" "user/*"`); strings.Contains(out, "alice") {
		t.Errorf("ungranted caller discovered the owner:\n%s", out)
	}

	// Revoke: discovery disappears with the grant.
	if !strings.Contains(a.cmd(`DELETEACL user/alice/INBOX bob`), "OK") {
		t.Fatal("revoke failed")
	}
	if out := b.cmd(`LIST "" "user/*"`); strings.Contains(out, "alice") {
		t.Errorf("revoked grant still discoverable:\n%s", out)
	}
}

// A stale registry row -- the grant gone from the ACL, the dict not yet
// reconciled -- and an invented owner produce the same silence: discovery
// never overrides the gate (#1138).
func TestOwnerRegistry_StaleRowIsSilent(t *testing.T) {
	d, addr := startRegistryServer(t)

	// Plant rows by hand: one for a real owner who granted nothing, one for
	// an owner that does not resolve.
	ctx := context.Background()
	tx, err := d.Begin(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"shared/shared-boxes/user/bob/alice",
		"shared/shared-boxes/user/bob/nosuch",
	} {
		if err := tx.Set(k, []byte("1")); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		t.Fatalf("commit: %v %v", res, err)
	}

	b := orphanLogin(t, addr, "bob")
	out := b.cmd(`LIST "" "user/*"`)
	for _, name := range []string{"alice", "nosuch"} {
		if strings.Contains(out, name) {
			t.Errorf("stale registry row for %q leaked through the gate:\n%s", name, out)
		}
	}
}

// The ordinary sharing path: alice grants on HER OWN mailbox through the
// personal namespace. The file is the same yarilo-acl the templated space
// reads (the 7.6 fact), so discovery must see the grant too -- otherwise
// SELECT user/alice/Sent works while LIST user/* stays empty, the exact
// LIST/SELECT split #1139 closed, reopened for discovery.
func TestOwnerRegistry_PersonalGrantFeedsDiscovery(t *testing.T) {
	_, addr := startRegistryServer(t)

	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`CREATE Sent`), "OK") {
		t.Fatal("create failed")
	}
	// Plain SETACL in the personal namespace -- no user/ prefix anywhere.
	if !strings.Contains(a.cmd(`SETACL Sent bob lr`), "OK") {
		t.Fatal("personal grant failed")
	}

	b := orphanLogin(t, addr, "bob")
	out := b.cmd(`LIST "" "user/*"`)
	if !strings.Contains(out, `"user/alice/Sent"`) {
		t.Fatalf("personal-namespace grant invisible to discovery:\n%s", out)
	}
	// Revocation through the same personal path clears discovery too.
	if !strings.Contains(a.cmd(`DELETEACL Sent bob`), "OK") {
		t.Fatal("revoke failed")
	}
	if out := b.cmd(`LIST "" "user/*"`); strings.Contains(out, "alice") {
		t.Errorf("personal revoke left discovery behind:\n%s", out)
	}
}

// The inverse pin: when the templated namespace points at a DIFFERENT root
// than the personal store (StampOwnerLocation allows it), a personal grant is
// a grant on a mailbox the templated space cannot reach -- it must NOT feed
// discovery. The condition is "this store backs an owner-templated space for
// this account", not "this is the personal store".
func TestOwnerRegistry_DivergentRootPersonalGrantStaysLocal(t *testing.T) {
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	lookup := func(_ context.Context, owner string) (*mailboxpkg.UserInfo, error) {
		if owner != "alice" && owner != "bob" {
			return nil, &notFoundError{owner}
		}
		home := filepath.Join(root, owner)
		// The templated space lives under Shared/, NOT the personal root.
		return &mailboxpkg.UserInfo{
			Username: owner, Home: home, MailPath: filepath.Join(home, "Shared"), Driver: "maildir",
		}, nil
	}
	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailboxpkg.Resolver{Root: root, HomeTemplate: "%n"},
		Auth:         &enforcePassdb{users: map[string]string{"alice": "pw", "bob": "pw"}},
		ACLEnabled:   true,
		UserdbLookup: lookup,
		SharedDict:   d,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
			{Type: imapserver.NamespaceShared, Prefix: "user/%u/", Separator: '/', List: imapserver.ListChildren,
				Location: "maildir:%h/Shared"},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	addr := ln.Addr().String()

	a := orphanLogin(t, addr, "alice")
	a.cmd(`SELECT INBOX`)
	if !strings.Contains(a.cmd(`SETACL INBOX bob lr`), "OK") {
		t.Fatal("personal grant failed")
	}
	b := orphanLogin(t, addr, "bob")
	if out := b.cmd(`LIST "" "user/*"`); strings.Contains(out, "alice") {
		t.Errorf("a grant outside the templated tree fed discovery:\n%s", out)
	}
}
