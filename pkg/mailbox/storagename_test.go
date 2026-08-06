package mailbox

import "testing"

// The round trip is the property that matters: whatever the client named, the
// client must get back. Asserting the on-disk form alone would pin an encoding
// without proving it can be read.
func TestStorageNameRoundTrip(t *testing.T) {
	const escape = "^"
	names := []string{
		"Invoices.2026",
		"example.com",
		"lists.golang-nuts",
		"a^b",         // the escape character itself
		"^",           // nothing but the escape character
		"a^2eb",       // text that looks like an escape sequence
		".hidden",     // leading dot: meaningful to the filesystem
		"~tilde",      // leading tilde
		"cur",         // a name the layout owns
		"Cur",         // and its case variants
		"dbox-Mails",  // the dbox marker
		"ordinary",    // untouched
		"Ünïcödé.txt", // non-ASCII around an escaped byte
	}

	for _, layoutSep := range []string{".", "/"} {
		for _, name := range names {
			esc := EscapeStorageName(name, layoutSep, escape)
			got := UnescapeStorageName(esc, escape)
			if got != name {
				t.Errorf("layout %q: %q escaped to %q, read back as %q", layoutSep, name, esc, got)
			}
		}
	}
}

// What must be escaped, spelled out: these are the bytes a layout would
// otherwise consume, and a name containing one is a name the client cannot get
// back without this.
func TestEscapeStorageNameEscapesWhatTheLayoutWouldConsume(t *testing.T) {
	const escape = "^"
	cases := []struct {
		name      string
		layoutSep string
		want      string
	}{
		{"Invoices.2026", ".", "Invoices^2e2026"},
		{"Invoices.2026", "/", "Invoices.2026"}, // "." is not the separator here
		{"a/b", ".", "a^2fb"},                   // "/" is always a level somewhere
		{"a^b", ".", "a^5eb"},
		{".hidden", ".", "^2ehidden"},
		{"~t", ".", "^7et"},
		// Nothing is escaped for a collision the layout cannot have: on
		// maildir++ the leading dot already keeps ".cur" from "cur".
		{"cur", ".", "cur"},
		{"CUR", ".", "CUR"},
		// The nested layout does own its marker.
		{"dbox-Mails", "/", "^64box-Mails"},
		{"current", ".", "current"}, // only a whole segment counts
		{"plain", ".", "plain"},
	}
	for _, tc := range cases {
		if got := EscapeStorageName(tc.name, tc.layoutSep, escape); got != tc.want {
			t.Errorf("EscapeStorageName(%q, %q) = %q, want %q", tc.name, tc.layoutSep, got, tc.want)
		}
	}
}

// Off by default: an empty escape character leaves every name exactly as it was,
// so enabling the feature is the only thing that changes any path.
func TestEscapingIsOffByDefault(t *testing.T) {
	for _, name := range []string{"Invoices.2026", "a/b", "cur", ".hidden", "a^b"} {
		if got := EscapeStorageName(name, ".", ""); got != name {
			t.Errorf("with no escape char, %q became %q", name, got)
		}
		if got := UnescapeStorageName(name, ""); got != name {
			t.Errorf("with no escape char, %q was decoded to %q", name, got)
		}
	}
}

// A name written before escaping was enabled, or by hand, must still list. The
// reverse pass leaves anything it cannot decode alone rather than dropping it.
func TestUnescapeLeavesUndecodableTextAlone(t *testing.T) {
	for _, s := range []string{"trailing^", "bad^zz", "^", "^2", "no escapes here"} {
		if got := UnescapeStorageName(s, "^"); got != s {
			t.Errorf("UnescapeStorageName(%q) = %q, want it left alone", s, got)
		}
	}
}

// Hierarchy the client asked for still becomes hierarchy; only a separator it
// wrote literally is escaped. This is the distinction the whole feature turns
// on, so it is asserted at the mapping rather than only end-to-end.
func TestFolderSubpathSeparatesHierarchyFromLiteralSeparators(t *testing.T) {
	const escape = "^"
	cases := []struct {
		driver, folder, sep, want string
	}{
		{"maildir", "a/b", "/", ".a.b"},                       // hierarchy
		{"maildir", "Invoices.2026", "/", ".Invoices^2e2026"}, // literal
		{"maildir", "a/b.c", "/", ".a.b^2ec"},                 // both at once
		{"mdbox", "a/b", "/", "mailboxes/a/b/dbox-Mails"},
		{"mdbox", "Invoices.2026", "/", "mailboxes/Invoices.2026/dbox-Mails"},
	}
	for _, tc := range cases {
		got := FolderSubpathEscaped(tc.driver, tc.folder, tc.folder, tc.sep, escape)
		if got != tc.want {
			t.Errorf("FolderSubpathEscaped(%q, %q, sep %q) = %q, want %q",
				tc.driver, tc.folder, tc.sep, got, tc.want)
		}
	}
}
