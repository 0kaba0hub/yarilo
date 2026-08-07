package mailbox

import (
	"strings"
	"testing"
)

// StampOwnerLocation must take the root and driver from the owner's userdb
// UserInfo and let the namespace template fill only the gaps. The distinguishing
// case is heterogeneous: an owner whose userdb driver (mdbox) and root differ
// from what the maildir template would produce -- on a homogeneous stand both
// give the same path and the test proves nothing.
func TestStampOwnerLocation(t *testing.T) {
	t.Run("userdb root and driver win over the template", func(t *testing.T) {
		owner := &UserInfo{Username: "alice", Home: "/h/alice", MailPath: "/m/alice", Driver: "mdbox"}
		got, err := StampOwnerLocation(owner, owner, "maildir:%h/Shared", '/')
		if err != nil {
			t.Fatal(err)
		}
		if got.MailPath != "/m/alice" {
			t.Errorf("MailPath = %q, want the userdb root /m/alice (template must not overwrite it)", got.MailPath)
		}
		if got.Driver != "mdbox" {
			t.Errorf("Driver = %q, want the userdb driver mdbox (template maildir must not win)", got.Driver)
		}
		if got.Separator != "/" {
			t.Errorf("Separator = %q, want /", got.Separator)
		}
		// The input is not mutated (callers may hold it in a cache).
		if owner.Separator != "" {
			t.Errorf("input owner mutated: Separator = %q", owner.Separator)
		}
	})

	t.Run("template fills a root the userdb left empty", func(t *testing.T) {
		owner := &UserInfo{Username: "bob", Driver: "maildir"}
		got, err := StampOwnerLocation(owner, owner, "maildir:/tmpl/bob", '/')
		if err != nil {
			t.Fatal(err)
		}
		if got.MailPath != "/tmpl/bob" || got.Home != "/tmpl/bob" {
			t.Errorf("root = (MailPath %q, Home %q), want /tmpl/bob from the template", got.MailPath, got.Home)
		}
	})

	t.Run("a location resolving to no path is an error", func(t *testing.T) {
		owner := &UserInfo{Username: "c", Home: "/h/c"}
		if _, err := StampOwnerLocation(owner, owner, "", '/'); err == nil {
			t.Error("empty location accepted; want an error")
		}
	})
}

func TestNamespaceFileSlug(t *testing.T) {
	cases := []struct{ prefix, sep, nsType, want string }{
		// The common shapes are unchanged, so no deployment renames a file it
		// already has.
		{"Public/", "/", "shared", "public"},
		{"Shared/", "/", "shared", "shared"},
		{"", "/", "personal", "personal"},
		// A separator inside the prefix must not become a path (#1159).
		{"Public/Team/", "/", "shared", "public-team"},
		{"Shared.Team.", ".", "shared", "shared-team"},
		// Everything from the first variable is dropped: it varies per user,
		// while this names a file inside one store.
		{"user/%u/", "/", "shared", "user"},
		{"people/%u/", "/", "shared", "people"},
		// A prefix that is nothing but a variable falls back to the type.
		{"%u/", "/", "shared", "shared"},
	}
	for _, c := range cases {
		if got := NamespaceFileSlug(c.prefix, c.sep, c.nsType); got != c.want {
			t.Errorf("NamespaceFileSlug(%q, %q, %q) = %q, want %q", c.prefix, c.sep, c.nsType, got, c.want)
		}
	}
	// Whatever the prefix, the result is one path segment.
	for _, p := range []string{"a/b/c/", "x\\y/", "user/%u/", "Public/Team/Sub/"} {
		if got := NamespaceFileSlug(p, "/", "shared"); strings.ContainsAny(got, `/\`) {
			t.Errorf("NamespaceFileSlug(%q) = %q, which is a path, not a filename", p, got)
		}
	}
}
