package mailbox_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// One mailbox addressed in two Unicode forms must be one directory in every
// tree derived from its name. The mail drivers normalised before building their
// path; the index, volatile, ACL and FTS trees passed the logical name through,
// so the same mailbox was one directory on the mail side and two on each of the
// others -- a second UID space and a fresh UIDVALIDITY for the index, a split
// index for FTS (#1092).
//
// The comparison is of the computed strings, not of directories: macOS
// normalises names on creation, so a filesystem probe shows the two trees
// agreeing when the paths do not.
func TestFolderSubpathIsOneNameInEveryForm(t *testing.T) {
	const (
		composed   = "Caf\u00e9"       // é as one rune
		decomposed = "Cafe\u0301"      // e + combining acute
		nested     = "Work/Caf\u00e9"  // the same, one level down
		nestedNFD  = "Work/Cafe\u0301" //
	)
	if composed == decomposed {
		t.Fatal("the two spellings are the same string; the fixture proves nothing")
	}

	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		for _, pair := range [][2]string{{composed, decomposed}, {nested, nestedNFD}} {
			a := mailbox.FolderSubpathForm(driver, pair[0], pair[0], "/", "", false)
			b := mailbox.FolderSubpathForm(driver, pair[1], pair[1], "/", "", false)
			if a != b {
				t.Errorf("%s: %q -> %q and %q -> %q; one mailbox, two directories",
					driver, pair[0], a, pair[1], b)
			}
			if !strings.Contains(a, composed) {
				t.Errorf("%s: %q is not the NFC form: %q", driver, a, composed)
			}
		}
	}
}

// With mailbox_list_normalize_to_nfc turned off, the two forms stay apart --
// the knob still means something, and the fix is not "always normalise".
func TestFolderSubpathKeepsFormsApartWhenNormalisationIsOff(t *testing.T) {
	const (
		composed   = "Caf\u00e9"  // é as one rune
		decomposed = "Cafe\u0301" // e + combining acute
	)
	a := mailbox.FolderSubpathForm("maildir", composed, composed, "/", "", true)
	b := mailbox.FolderSubpathForm("maildir", decomposed, decomposed, "/", "", true)
	if a == b {
		t.Errorf("normalisation is disabled and both forms still gave %q", a)
	}
}

// The default path -- what a caller that passes no form gets -- normalises,
// because the config key defaults to true and the zero value has to agree with
// it. A field spelled NormalizeNFC would have made every caller that forgot it
// disable normalisation silently, which is the defect, not the fix.
func TestFolderSubpathDefaultsToNormalising(t *testing.T) {
	const (
		composed   = "Caf\u00e9"  // é as one rune
		decomposed = "Cafe\u0301" // e + combining acute
	)
	if got, want := mailbox.FolderSubpathEscaped("maildir", decomposed, decomposed, "/", ""),
		mailbox.FolderSubpathEscaped("maildir", composed, composed, "/", ""); got != want {
		t.Errorf("default form gave %q for NFD and %q for NFC", got, want)
	}
}
