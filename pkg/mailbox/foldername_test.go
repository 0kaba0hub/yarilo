package mailbox

import "testing"

// The names that mattered, and why each one did.
func TestValidateFolderName(t *testing.T) {
	for _, tc := range []struct {
		name string
		sep  string
		ok   bool
		why  string
	}{
		{"", "/", false, "resolves to the mailbox root — Delete removed every message and the index"},
		{".", "/", false, "resolves above the root — Delete removed the user's home directory"},
		{"..", "/", false, "climbs out of the mail tree"},
		{"a/..", "/", false, "climbs back out after descending"},
		{"../x", "/", false, "names a sibling account's tree"},
		{"a/./b", "/", false, "a no-op segment is still a segment"},
		{"./../victim/Maildir", ".", false, "read another account's mail on a dot-separator deployment"},
		{"x\x00y", "/", false, "a NUL truncates the path at the syscall boundary"},

		{"Work", "/", true, ""},
		{"Work/Reports", "/", true, ""},
		{"Work.Reports", ".", true, ""},
		{"..bin", "/", true, "leading dots are ordinary characters, not segments"},
		{"a..b", "/", true, ""},
		{"INBOX", "/", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFolderName(tc.name, tc.sep)
			if ok := err == nil; ok != tc.ok {
				t.Errorf("ValidateFolderName(%q, %q) error = %v, want ok=%v — %s",
					tc.name, tc.sep, err, tc.ok, tc.why)
			}
		})
	}
}

// Both separators are examined whatever is configured, because the name has to
// be safe as a path on the way in and the on-disk separator is not always the
// IMAP one. A deployment using "/" was protected only by that rewrite, which is
// configuration rather than a guarantee.
func TestValidateFolderNameChecksBothSeparators(t *testing.T) {
	if err := ValidateFolderName("a/../b", "."); err == nil {
		t.Error(`"a/../b" accepted with a "." separator; "/" is still a path separator on disk`)
	}
	if err := ValidateFolderName("a.../b", "/"); err != nil {
		t.Errorf(`"a.../b" refused with a "/" separator: %v — "..." is not a traversal segment`, err)
	}
}
