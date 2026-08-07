package mailbox_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const (
	nfcComposed = "Café"  // é as one rune
	nfcDecomp   = "Café" // e + combining acute
)

// The path builder no longer normalises: NFC lives at the name-entry boundary
// now, so FolderSubpathEscaped takes the name exactly as given. Two spellings
// therefore give two paths here — which is correct, because by the time a name
// reaches this function it has already been through NormalizeName once.
//
// This is the inverse of the old #1092 assertion, and deliberately so: proving
// the builder is form-preserving is what proves normalisation is not hiding in
// it as a second owner (#1113).
func TestFolderSubpathIsFormPreserving(t *testing.T) {
	if nfcComposed == nfcDecomp {
		t.Fatal("the two spellings are the same string; the fixture proves nothing")
	}
	a := mailbox.FolderSubpathEscaped("maildir", nfcComposed, nfcComposed, "/", "")
	b := mailbox.FolderSubpathEscaped("maildir", nfcDecomp, nfcDecomp, "/", "")
	if a == b {
		t.Errorf("the path builder normalised %q and %q to one path; NFC must live at the boundary, not here", nfcComposed, nfcDecomp)
	}
}

// The property that used to belong to the builder now belongs to the boundary
// plus the builder: normalise first, and the two spellings become one path in
// every tree. This is what the four trees actually rely on, now expressed
// through the single owner rather than smuggled into path derivation.
func TestNormalizeThenSubpathIsOneNameInEveryForm(t *testing.T) {
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		ca := mailbox.NormalizeName(nfcComposed, false)
		cb := mailbox.NormalizeName(nfcDecomp, false)
		a := mailbox.FolderSubpathEscaped(driver, ca, ca, "/", "")
		b := mailbox.FolderSubpathEscaped(driver, cb, cb, "/", "")
		if a != b {
			t.Errorf("%s: normalised forms still gave %q and %q", driver, a, b)
		}
		if !strings.Contains(a, nfcComposed) {
			t.Errorf("%s: %q is not the NFC form", driver, a)
		}
	}
}

// NormalizeName is the knob: skip leaves the name exactly as written, so a
// deployment with mailbox_list_normalize_to_nfc off keeps the two forms apart.
func TestNormalizeNameHonoursTheSkipFlag(t *testing.T) {
	if got := mailbox.NormalizeName(nfcDecomp, false); got != nfcComposed {
		t.Errorf("normalise on: %q, want the composed form", got)
	}
	if got := mailbox.NormalizeName(nfcDecomp, true); got != nfcDecomp {
		t.Errorf("normalise off: %q, want the decomposed form unchanged", got)
	}
}
