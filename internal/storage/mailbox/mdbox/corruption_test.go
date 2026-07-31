package mdbox

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// openTestUserMailboxAlt is openTestUserMailbox with an alt-storage tier rooted
// under altHome.
func openTestUserMailboxAlt(t *testing.T, home, altHome string) *userMailbox {
	t.Helper()
	b := New(WithAltStorage(filepath.Join(altHome, "%u")))
	u := b.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return u
}

func mustSave(t *testing.T, u *userMailbox, body string) string {
	t.Helper()
	fn, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return fn
}

// TestFetchClassifiesCorruption verifies Fetch wraps mailbox.ErrCorruptStorage
// for the three structural-corruption cases (map miss, vanished m.<N>, truncated
// record) so the reactive rebuild can distinguish them from transient I/O.
func TestFetchClassifiesCorruption(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	fn := mustSave(t, u, "From: a@a.com\r\nSubject: x\r\n\r\nhello world body\r\n")

	// Sanity: a healthy Fetch works.
	rc, err := u.Fetch("INBOX", fn, false)
	if err != nil {
		t.Fatalf("healthy fetch: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	t.Run("map miss", func(t *testing.T) {
		// A map_uid the map never assigned = fileindex/map divergence.
		_, err := u.Fetch("INBOX", "999999", false)
		if !errors.Is(err, mailbox.ErrCorruptStorage) {
			t.Fatalf("map miss: got %v, want ErrCorruptStorage", err)
		}
	})

	t.Run("vanished m.N", func(t *testing.T) {
		if err := os.Remove(u.mfilePath(1)); err != nil {
			t.Fatal(err)
		}
		_, err := u.Fetch("INBOX", fn, false)
		if !errors.Is(err, mailbox.ErrCorruptStorage) {
			t.Fatalf("vanished file: got %v, want ErrCorruptStorage", err)
		}
	})
}

// TestFetchTruncatedIsCorrupt truncates the m.<N> mid-record so the body read
// hits ErrUnexpectedEOF, which must classify as corruption.
func TestFetchTruncatedIsCorrupt(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	fn := mustSave(t, u, "From: a@a.com\r\nSubject: x\r\n\r\n"+strings.Repeat("body ", 40)+"\r\n")

	path := u.mfilePath(1)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Halve the file so the cut lands inside the record body (past the header,
	// short of the trailer), forcing an ErrUnexpectedEOF on the body read.
	if err := os.Truncate(path, st.Size()/2); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Fetch("INBOX", fn, false); !errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Fatalf("truncated fetch: got %v, want ErrCorruptStorage", err)
	}
}

// TestScanQuarantinesCorruptTail verifies a corrupt record does not abort the
// scan: the good prefix survives and scanStorage returns no error.
func TestScanQuarantinesCorruptTail(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	mustSave(t, u, "From: a@a.com\r\nSubject: good\r\n\r\nvalid record body\r\n")

	// Append 100 bytes with no LF right after the valid record: the next record's
	// file-header line can never terminate, so the walk quarantines from there.
	f, err := os.OpenFile(u.mfilePath(1), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(strings.Repeat("X", 100))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	recs, err := u.scanStorage()
	// The good prefix survives AND the scan is reported incomplete so a
	// destructive consumer aborts rather than expunging the unread tail.
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("got err %v, want ErrScanIncomplete", err)
	}
	if !errors.Is(err, errScanCorrupt) {
		t.Fatalf("incomplete cause should chain errScanCorrupt, got %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("kept %d records, want 1 (good prefix before corruption)", len(recs))
	}
}

// TestScanFullyCorruptFileIsEmpty verifies a file whose very first record is
// broken yields zero records for that file without aborting the whole scan.
func TestScanFullyCorruptFileIsEmpty(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	if _, err := u.openMap(); err != nil {
		t.Fatal(err)
	}
	// A garbage m.1 with no LF in the first 64 bytes.
	if err := os.WriteFile(u.mfilePath(1), []byte(strings.Repeat("Z", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := u.scanStorage()
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("got err %v, want ErrScanIncomplete", err)
	}
	if len(recs) != 0 {
		t.Fatalf("got %d records from a fully-corrupt file, want 0", len(recs))
	}
}

// TestScanReadErrClassifies verifies a truncated read is structural corruption
// while any other read failure (EIO/ESTALE) is transient I/O — the split that
// keeps a flaky disk from being mistaken for vanished mail.
func TestScanReadErrClassifies(t *testing.T) {
	if err := scanReadErr(io.ErrUnexpectedEOF, "body"); !errors.Is(err, errScanCorrupt) || errors.Is(err, errScanIO) {
		t.Errorf("truncation: got %v, want errScanCorrupt (not errScanIO)", err)
	}
	if err := scanReadErr(errors.New("input/output error"), "body"); !errors.Is(err, errScanIO) || errors.Is(err, errScanCorrupt) {
		t.Errorf("EIO: got %v, want errScanIO (not errScanCorrupt)", err)
	}
}

// TestMdboxIsFolderAgnostic pins the contract the operator rebuild endpoint
// relies on to reject mdbox: its scan is storage-wide, so a per-folder rebuild
// is unsafe.
func TestMdboxIsFolderAgnostic(t *testing.T) {
	u := openTestUserMailbox(t, filepath.Join(t.TempDir(), "home"))
	fa, ok := mailbox.UserMailbox(u).(mailbox.FolderAgnosticStorage)
	if !ok || !fa.FolderAgnosticScan() {
		t.Fatalf("mdbox must satisfy FolderAgnosticStorage and report true (ok=%v)", ok)
	}
}

// TestScanIncludesAltTier verifies scanStorage walks the alt directory, not just
// primary — an alt-only m.<N> (cold tier) must not be silently dropped.
func TestScanIncludesAltTier(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	altHome := filepath.Join(base, "alt")
	u := openTestUserMailboxAlt(t, home, altHome)
	mustSave(t, u, "From: a@a.com\r\nSubject: cold\r\n\r\nalt-resident body\r\n")

	// Physically move m.1 from primary to the alt tier.
	if err := os.MkdirAll(u.altStoragePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(u.mfilePath(1), u.mfileAltPath(1)); err != nil {
		t.Fatal(err)
	}

	recs, err := u.scanStorage()
	if err != nil {
		t.Fatalf("scanStorage: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records after moving m.1 to alt, want 1 (alt dir must be scanned)", len(recs))
	}
}
