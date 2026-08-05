package mailbox

import (
	"errors"
	"strings"
	"testing"
)

func TestGuardDestructivePath(t *testing.T) {
	const root = "/var/mail/u@d.test/Maildir"
	cases := []struct {
		name    string
		path    string
		refused bool
	}{
		{"a folder inside the root", root + "/.Archive", false},
		{"a nested folder", root + "/.Work.2026", false},
		{"the root itself", root, true},
		{"the root with a trailing slash", root + "/", true},
		{"the root reached the long way", root + "/.Archive/..", true},
		{"the parent", "/var/mail/u@d.test", true},
		{"another account", "/var/mail/other@d.test/Maildir/.Archive", true},
		{"an unrelated path", "/etc", true},
		// A sibling whose name merely begins with the root's: string-prefix
		// containment without the separator would let this through.
		{"a sibling sharing the prefix", root + "2/.Archive", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardDestructivePath(root, tc.path)
			switch {
			case tc.refused && err == nil:
				t.Errorf("GuardDestructivePath(%q) allowed it", tc.path)
			case !tc.refused && err != nil:
				t.Errorf("GuardDestructivePath(%q) = %v, want allowed", tc.path, err)
			case tc.refused && !errors.Is(err, ErrPathIsRoot):
				t.Errorf("refusal of %q does not carry ErrPathIsRoot: %v", tc.path, err)
			}
		})
	}
}

// A driver that cannot say where its root is cannot be guarded. Refusing
// everything there would disable the operations rather than bound them, and
// pretending to guard is worse than not guarding.
func TestGuardWithoutARootAllows(t *testing.T) {
	if err := GuardDestructivePath("", "/anywhere/at/all"); err != nil {
		t.Errorf("GuardDestructivePath with no root = %v, want allowed", err)
	}
}

// Rename has two paths and either landing on the root is the same fault.
func TestGuardDestructivePathsChecksEvery(t *testing.T) {
	const root = "/var/mail/u/Maildir"
	if err := GuardDestructivePaths(root, root+"/.A", root+"/.B"); err != nil {
		t.Errorf("two ordinary folders were refused: %v", err)
	}
	if err := GuardDestructivePaths(root, root+"/.A", root); err == nil {
		t.Error("a rename onto the root was allowed")
	}
	if err := GuardDestructivePaths(root, root, root+"/.B"); err == nil {
		t.Error("a rename of the root was allowed")
	}
}

// With a separate mail_path, the INBOX directory can sit outside the folder
// root. It is still the mailbox, and the refusal should say so: "outside the
// root" describes a misplaced folder, which is the wrong fault to report and
// the wrong thing for an operator to go looking for.
func TestGuardNamesTheMailboxRatherThanCallingItMisplaced(t *testing.T) {
	const root = "/var/mail/u/Maildir"
	const inbox = "/var/spool/inbox/u"

	err := GuardDestructivePath(root, inbox, inbox)
	if err == nil {
		t.Fatal("the INBOX directory was allowed")
	}
	if !errors.Is(err, ErrPathIsRoot) {
		t.Errorf("refusal does not carry ErrPathIsRoot: %v", err)
	}
	if !strings.Contains(err.Error(), "is the mailbox itself") {
		t.Errorf("refusal = %v; it should say the path is the mailbox, not that it is misplaced", err)
	}

	// An ordinary folder is unaffected by the extra root.
	if err := GuardDestructivePath(root, root+"/.Archive", inbox); err != nil {
		t.Errorf("an ordinary folder was refused: %v", err)
	}
}
