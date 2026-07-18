package mdbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// mdbox must satisfy the shared reactive-healer contract so the FSCKD marker
// gating (CanReactiveHeal) activates for it and IMAP/POP3 drive the heal through
// the same interface as sdbox.
var _ mailbox.ReactiveHealer = (*userMailbox)(nil)

// TestHealExpungesVanishedAndClearsMarker: a folder record whose message file
// vanished is expunged by the reactive heal, and the FSCKD marker is cleared.
func TestHealExpungesVanishedAndClearsMarker(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "will vanish\r\n")

	// Vanish the storage: delete m.1 (a clean scan then finds nothing present).
	if err := os.Remove(box.mfilePath(1)); err != nil {
		t.Fatal(err)
	}
	// Flag the folder as a prior read would have.
	f, _ := idx.OpenFolder("INBOX", 0)
	cm := idx.(mailbox.CorruptionMarker)
	if err := cm.MarkFolderCorrupt(f.ID); err != nil {
		t.Fatal(err)
	}

	expunged, err := box.HealCorruptFolder(idx, f)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if expunged != 1 {
		t.Errorf("expunged = %d, want 1", expunged)
	}
	if got := folderCount(t, idx, "INBOX"); got != 0 {
		t.Errorf("INBOX count = %d, want 0 (vanished record expunged)", got)
	}
	// Marker cleared in the same lock scope.
	f2, _ := idx.OpenFolder("INBOX", 0)
	if f2.Fsckd {
		t.Error("FSCKD marker should be cleared after a successful heal")
	}
}

// TestHealAbortsOnIncompleteScan: a structurally corrupt m.<N> (the same signal
// a concurrent purge/altmove race produces) makes the scan incomplete, so the
// heal ABORTS and expunges nothing — never mistaking an unreadable-right-now
// message for a vanished one.
func TestHealAbortsOnIncompleteScan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "good record\r\n")
	// Corrupt the tail so the scan quarantines and returns ErrScanIncomplete.
	fh, err := os.OpenFile(box.mfilePath(1), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.Write([]byte(strings.Repeat("X", 100)))
	_ = fh.Close()

	f, _ := idx.OpenFolder("INBOX", 0)
	_, err = box.HealCorruptFolder(idx, f)
	if err == nil {
		t.Fatal("heal should abort (return error) on an incomplete scan")
	}
	// Nothing expunged — the good record survives.
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX count = %d, want 1 (nothing dropped on abort)", got)
	}
}
