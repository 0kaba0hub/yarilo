package imap_test

import (
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
)

// The LIST reference selects what to match; it is not glued onto results.
//
// It was: every name from every namespace came back with the reference in
// front, so LIST "Public/" "*" answered with the user's own folders wearing a
// prefix of a namespace that does not exist here — names no command would
// accept afterwards (#1099).
func TestListReferenceIsNotPrefixedOntoResults(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("Work.2026", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	for _, name := range listNames(t, c, "Public/", "*") {
		t.Errorf("LIST \"Public/\" \"*\" returned %q; no such namespace exists here", name)
	}

	bare := listNames(t, c, "", "*")
	for _, name := range bare {
		if name == "INBOX" || name == "Work" || name == "Work.2026" {
			continue
		}
		t.Errorf("LIST \"\" \"*\" returned %q, which is not a mailbox of this user", name)
	}
}

// The wildcards RFC 9051 §6.3.9 defines have to work: only a bare "*" or "%"
// or an exact name matched before, so a client listing a subtree — the ordinary
// way to explore — was told it is empty.
func TestListWildcards(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// The default namespace separator here is ".", so nesting is written with
	// it; a "/" would be a literal character in the name.
	for _, f := range []string{"Work", "Work.2026", "Wardrobe"} {
		if err := c.Create(f, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		ref, pattern string
		want         []string
	}{
		{"", "*", []string{"INBOX", "Work", "Work.2026", "Wardrobe"}},
		{"", "W*", []string{"Work", "Work.2026", "Wardrobe"}},
		{"", "W%", []string{"Work", "Wardrobe"}}, // % stops at the separator
		{"", "Work.%", []string{"Work.2026"}},    // a subtree
		{"Work.", "%", []string{"Work.2026"}},    // the same, via the reference
		{"", "INBOX", []string{"INBOX"}},         // exact
		{"", "nosuch*", nil},                     // nothing
	}
	for _, tc := range cases {
		got := listNames(t, c, tc.ref, tc.pattern)
		if !sameSet(got, tc.want) {
			t.Errorf("LIST %q %q = %v, want %v", tc.ref, tc.pattern, got, tc.want)
		}
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

func listNames(t *testing.T, c *imapclient.Client, ref, pattern string) []string {
	t.Helper()
	data, err := c.List(ref, pattern, nil).Collect()
	if err != nil {
		t.Fatalf("LIST %q %q: %v", ref, pattern, err)
	}
	out := make([]string, 0, len(data))
	for _, d := range data {
		out = append(out, d.Mailbox)
	}
	return out
}

// Mailbox names are case-sensitive; INBOX is the exception RFC 9051 §5.1
// names. Folding both sides in the matcher would have been a behaviour change
// riding in with the rewrite — quiet, because every test used matching case.
func TestListMatchingIsCaseSensitiveExceptINBOX(t *testing.T) {
	c := startServerWith(t, maildirBackend(t))
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	if got := listNames(t, c, "", "work"); len(got) != 0 {
		t.Errorf(`LIST "" "work" returned %v; mailbox names are case-sensitive`, got)
	}
	if got := listNames(t, c, "", "Work"); len(got) != 1 {
		t.Errorf(`LIST "" "Work" returned %v, want the folder itself`, got)
	}
	for _, spelling := range []string{"inbox", "InBoX", "INBOX"} {
		if got := listNames(t, c, "", spelling); len(got) != 1 {
			t.Errorf("LIST %q returned %v; INBOX is case-insensitive", spelling, got)
		}
	}
}
