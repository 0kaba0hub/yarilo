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

func TestNamespaceKeepsSubscriptions(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		nsType   string
		prefix   string
		explicit *bool
		want     bool
	}{
		// Personal keeps its own file regardless of the setting.
		{"personal default", "personal", "", nil, true},
		{"personal explicit false still keeps", "personal", "", &no, true},
		// Fixed shared/public keep their own file unless the operator
		// delegates: an upgrade must move nothing, and a site-wide file has
		// no correct per-user migration.
		{"shared default keeps", "shared", "Shared/", nil, true},
		{"public default keeps", "public", "Public/", nil, true},
		{"shared explicit true keeps", "shared", "Shared/", &yes, true},
		{"shared explicit false delegates", "shared", "Shared/", &no, false},
		// Owner-templated never keeps one; explicit true is refused at
		// startup by pkg/config, so it must not flip the answer here.
		{"owner-templated default", "shared", "user/%u/", nil, false},
		{"owner-templated explicit true still no", "shared", "user/%u/", &yes, false},
	}
	for _, c := range cases {
		if got := NamespaceKeepsSubscriptions(c.nsType, c.prefix, c.explicit); got != c.want {
			t.Errorf("%s: NamespaceKeepsSubscriptions(%q, %q) = %v, want %v", c.name, c.nsType, c.prefix, got, c.want)
		}
	}
}

func TestNamespaceListMode(t *testing.T) {
	cases := []struct {
		prefix, raw string
		want        string
		ok          bool
	}{
		// Unset takes the kind default: the template node is not a mailbox.
		{"", "", "yes", true},
		{"Public/", "", "yes", true},
		{"user/%u/", "", "children", true},
		// One vocabulary: bool spellings are rejected, or "true" would mean
		// yes forever against three states. YAML true/false arrive here as
		// the weakly-typed "1"/"0", so those four are the exact inputs a
		// list: true config produces.
		{"Public/", "true", "", false},
		{"Public/", "1", "", false},
		{"Public/", "false", "", false},
		{"Public/", "0", "", false},
		{"user/%u/", "yes", "yes", true},
		{"Public/", "children", "children", true},
		{"Public/", "CHILDREN", "children", true},
		{"Public/", "maybe", "", false},
	}
	for _, c := range cases {
		got, ok := NamespaceListMode(c.prefix, c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("NamespaceListMode(%q, %q) = %q,%v want %q,%v", c.prefix, c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestAdvertisedPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Public/", "Public/"},
		{"user/%u/", "user/"},
		{"user/%u", "user/"},
		{"people/%n@%d/", "people/"},
		{"%u/", ""},
	}
	for _, c := range cases {
		if got := AdvertisedPrefix(c.in); got != c.want {
			t.Errorf("AdvertisedPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
