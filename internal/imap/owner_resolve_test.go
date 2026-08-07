package imap

import (
	"context"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fakeUserdb returns a per-owner UserInfo the way the auth-master lookup would:
// home from a template, driver already stamped from the owner's mail_location.
func fakeUserdb(m map[string]*mailbox.UserInfo) func(context.Context, string) (*mailbox.UserInfo, error) {
	return func(_ context.Context, u string) (*mailbox.UserInfo, error) {
		if ui, ok := m[u]; ok {
			// Return a copy so the producer's mutations do not leak between calls.
			c := *ui
			return &c, nil
		}
		return nil, errNoSuchUser
	}
}

var errNoSuchUser = &imapError{"no such user"}

type imapError struct{ s string }

func (e *imapError) Error() string { return e.s }

func TestResolveOwnerUserInfo(t *testing.T) {
	spec := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/', Location: "maildir:%h/shared"}
	base := &mailbox.UserInfo{Username: "caller", StorageEscapeChar: "^", SkipNFCNormalize: true}

	// alice: userdb says mdbox, home /srv/alice.
	db := fakeUserdb(map[string]*mailbox.UserInfo{
		"alice": {Username: "alice", Home: "/srv/alice", MailPath: "/srv/alice", Driver: "mdbox"},
		"bob":   {Username: "bob", Home: "/srv/bob", MailPath: "/srv/bob", Driver: "maildir"},
	})

	t.Run("driver from userdb wins the namespace template", func(t *testing.T) {
		ui, err := resolveOwnerUserInfo(context.Background(), db, base, spec, "alice")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// spec.Location is maildir:..., userdb says mdbox -> mdbox wins.
		if ui.Driver != "mdbox" {
			t.Errorf("Driver = %q, want mdbox (userdb wins the namespace location's maildir)", ui.Driver)
		}
	})

	t.Run("root from the userdb, not the namespace template", func(t *testing.T) {
		ui, err := resolveOwnerUserInfo(context.Background(), db, base, spec, "alice")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// userdb gave MailPath /srv/alice; the namespace template maildir:%h/shared
		// must NOT overwrite it -- the owner's real store wins, so a per-user
		// driver is never pointed at a template path.
		if ui.MailPath != "/srv/alice" {
			t.Errorf("MailPath = %q, want /srv/alice (userdb root over the template)", ui.MailPath)
		}
	})

	t.Run("template fills the root only when the userdb gave none", func(t *testing.T) {
		// An owner whose userdb returns no MailPath falls back to the template.
		noPath := fakeUserdb(map[string]*mailbox.UserInfo{
			"carol": {Username: "carol", Home: "/srv/carol", Driver: "maildir"},
		})
		ui, err := resolveOwnerUserInfo(context.Background(), noPath, base, spec, "carol")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ui.MailPath != "/srv/carol/shared" {
			t.Errorf("MailPath = %q, want /srv/carol/shared (template fills the empty userdb root)", ui.MailPath)
		}
	})

	t.Run("identity: owner name, deployment-wide storage form from base", func(t *testing.T) {
		ui, _ := resolveOwnerUserInfo(context.Background(), db, base, spec, "alice")
		if ui.Username != "alice" {
			t.Errorf("Username = %q, want the owner alice", ui.Username)
		}
		if ui.StorageEscapeChar != "^" || !ui.SkipNFCNormalize {
			t.Errorf("storage-name form not carried from base: %q %v", ui.StorageEscapeChar, ui.SkipNFCNormalize)
		}
	})

	t.Run("two owners resolve to two roots", func(t *testing.T) {
		a, _ := resolveOwnerUserInfo(context.Background(), db, base, spec, "alice")
		b, _ := resolveOwnerUserInfo(context.Background(), db, base, spec, "bob")
		if a.MailPath == b.MailPath {
			t.Errorf("two owners share a root %q; the template does not distinguish them", a.MailPath)
		}
	})

	t.Run("unknown owner is an error, never a UserInfo with an empty name", func(t *testing.T) {
		ui, err := resolveOwnerUserInfo(context.Background(), db, base, spec, "nobody")
		if err == nil {
			t.Fatalf("unknown owner resolved to %+v; want an error", ui)
		}
		if ui != nil {
			t.Errorf("error path returned a UserInfo: %+v", ui)
		}
	})

	t.Run("nil lookup is refused, not treated as no owner", func(t *testing.T) {
		if _, err := resolveOwnerUserInfo(context.Background(), nil, base, spec, "alice"); err == nil {
			t.Error("nil lookup accepted")
		}
	})
}

// Modifier precedence: the userdb's INDEX= wins where it set one; the namespace
// location fills only the gaps. Asserted with a fixed answer, not "some value".
func TestResolveOwnerUserInfoModifierOrder(t *testing.T) {
	spec := NamespaceSpec{Type: NamespaceShared, Prefix: "user/%u/", Separator: '/',
		Location: "maildir:%h/shared:INDEX=/ns-index:CONTROL=/ns-control"}
	base := &mailbox.UserInfo{Username: "caller"}
	db := fakeUserdb(map[string]*mailbox.UserInfo{
		// userdb set IndexDir, left ControlDir empty.
		"alice": {Username: "alice", Home: "/srv/alice", Driver: "mdbox", IndexDir: "/srv/alice/userdb-index"},
	})
	ui, err := resolveOwnerUserInfo(context.Background(), db, base, spec, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ui.IndexDir != "/srv/alice/userdb-index" {
		t.Errorf("IndexDir = %q, want the userdb value (first-writer-wins)", ui.IndexDir)
	}
	if !strings.Contains(ui.ControlDir, "ns-control") {
		t.Errorf("ControlDir = %q, want the namespace value filling the gap the userdb left", ui.ControlDir)
	}
}
