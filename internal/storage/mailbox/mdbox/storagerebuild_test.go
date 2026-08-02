package mdbox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// saveUntaggedOrphan writes a record with NO ORIG_MAILBOX tag (simulating a
// message stored before the key existed) and records it in the map, referenced
// by no folder. Returns its map_uid string.
func saveUntaggedOrphan(t *testing.T, box *userMailbox, body string) string {
	t.Helper()
	m, err := box.openMap()
	if err != nil {
		t.Fatal(err)
	}
	guid := randomGUID()
	rec := buildDboxRecord([]byte(body), guid, "") // empty origMailbox => untagged
	fileID := m.HighestFileID()
	if fileID == 0 {
		fileID = 1
	}
	f, err := os.OpenFile(box.mfilePath(fileID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	offset := uint32(st.Size())
	if _, err := f.Write(rec); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	mapUID, err := m.AppendRecord(fileID, offset, uint32(len(rec)), guid)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatUint(uint64(mapUID), 10)
}

// newBoxAndIndex wires a real mdbox box and a real file index over the same
// home, so the storage-wide rebuild can be exercised end to end.
func newBoxAndIndex(t *testing.T, home string, opts ...Option) (*userMailbox, mailbox.UserIndex) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@x.io", Home: home}
	box := New(opts...).OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatalf("box init: %v", err)
	}
	idx := file.New().OpenUser(info)
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open INBOX index: %v", err)
	}
	return box, idx
}

func deliverMsg(t *testing.T, box *userMailbox, idx mailbox.UserIndex, folder, body string) uint32 {
	t.Helper()
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatalf("open %s: %v", folder, err)
	}
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		t.Fatalf("alloc uid: %v", err)
	}
	fn, _, err := box.Save(folder, strings.NewReader(body), uid, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: fn, Size: uint32(len(body))}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return uid
}

func folderCount(t *testing.T, idx mailbox.UserIndex, folder string) int {
	t.Helper()
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatalf("open %s: %v", folder, err)
	}
	msgs, err := idx.GetMessages(f.ID, allMessages)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	return len(msgs)
}

// TestTrailerOrigMailboxRoundTrip verifies the ORIG_MAILBOX key survives a
// build/scan round-trip and that an UNKNOWN trailer key is skipped without
// breaking framing (an older reader ignores a key it does not know).
func TestTrailerOrigMailboxRoundTrip(t *testing.T) {
	// A tagged record, then an untagged one, packed back-to-back. If the tag
	// broke framing, the second record would not parse at the right offset.
	var buf bytes.Buffer
	buf.Write(buildDboxRecord([]byte("first body\r\n"), randomGUID(), "Archive/2024"))
	secondOff := buf.Len()
	buf.Write(buildDboxRecord([]byte("second body\r\n"), randomGUID(), ""))

	dir := t.TempDir()
	path := filepath.Join(dir, "m.1")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &userMailbox{}
	recs, err := u.scanMFileAt(path)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("parsed %d records, want 2 (framing broken by the tag?)", len(recs))
	}
	if recs[0].scan.OrigMailbox != "Archive/2024" {
		t.Errorf("orig mailbox = %q, want Archive/2024", recs[0].scan.OrigMailbox)
	}
	if recs[1].scan.OrigMailbox != "" {
		t.Errorf("untagged record orig mailbox = %q, want empty", recs[1].scan.OrigMailbox)
	}
	if int(recs[1].physicalOffset) != secondOff {
		t.Errorf("second record at offset %d, want %d", recs[1].physicalOffset, secondOff)
	}

	// An unknown trailer key (a future reader's field) must be skipped, leaving
	// the known keys intact.
	var raw bytes.Buffer
	raw.WriteString(magicPost)
	raw.WriteString("Gffffffffffffffffffffffffffffffff\n")
	raw.WriteString("Zsome-future-field\n") // unknown key
	raw.WriteString("BShared/Team\n")
	raw.WriteByte('\n')
	_, parsed, err := scanTrailer(bytes.NewReader(raw.Bytes()), uint32(raw.Len()))
	if err != nil {
		t.Fatalf("scanTrailer with unknown key: %v", err)
	}
	if parsed.origMailbox != "Shared/Team" {
		t.Errorf("orig mailbox after unknown key = %q, want Shared/Team", parsed.origMailbox)
	}
}

// TestRebuildRestoresTaggedOrphanOptIn: with restore_orphans=true a present,
// unreferenced, ORIG_MAILBOX-tagged message is re-filed into its home folder;
// an UNTAGGED unreferenced message (the live-sandbox 889 case) is NOT restored.
func TestRebuildRestoresTaggedOrphanOptIn(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "referenced\r\n")

	// Tagged orphan: saved into "Archive" (so its trailer records Archive) but
	// never appended to any folder index.
	if _, _, err := box.Save("Archive", strings.NewReader("tagged orphan\r\n"), 0, 14, nil); err != nil {
		t.Fatalf("save tagged orphan: %v", err)
	}
	// Untagged orphan: no ORIG_MAILBOX at all.
	untaggedUID, _ := parseFilename(saveUntaggedOrphan(t, box, "untagged orphan\r\n"))

	stats, err := box.RebuildStorage(idx, true)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.OrphansRestored != 1 {
		t.Errorf("orphans restored = %d, want 1 (only the tagged one)", stats.OrphansRestored)
	}
	if got := folderCount(t, idx, "Archive"); got != 1 {
		t.Errorf("Archive count = %d, want 1 (tagged orphan restored home)", got)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX count = %d, want 1 (nothing dumped into INBOX)", got)
	}
	// The untagged orphan is NOT restored — it is zero-ref for purge.
	zeroRef := map[uint32]bool{}
	m, _ := box.openMap()
	for _, uid := range m.CompactGarbage() {
		zeroRef[uid] = true
	}
	if !zeroRef[untaggedUID] {
		t.Errorf("untagged orphan %d must be zero-ref (never restored)", untaggedUID)
	}
}

// TestRebuildDefaultDoesNotRestore: with restore_orphans=false (default) even a
// tagged orphan is left zero-ref — the default run is identical to before.
func TestRebuildDefaultDoesNotRestore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "referenced\r\n")
	taggedUID, _ := parseFilename(func() string {
		fn, _, err := box.Save("Archive", strings.NewReader("tagged orphan\r\n"), 0, 14, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fn
	}())

	stats, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.OrphansRestored != 0 {
		t.Errorf("orphans restored = %d, want 0 (restore disabled)", stats.OrphansRestored)
	}
	// Archive folder was never created / populated by a default run.
	if exists, _ := box.FolderExists("Archive"); exists {
		if got := folderCount(t, idx, "Archive"); got != 0 {
			t.Errorf("Archive count = %d, want 0 (no restore by default)", got)
		}
	}
	zeroRef := map[uint32]bool{}
	m, _ := box.openMap()
	for _, uid := range m.CompactGarbage() {
		zeroRef[uid] = true
	}
	if !zeroRef[taggedUID] {
		t.Errorf("tagged orphan %d must be zero-ref when restore is off", taggedUID)
	}
}

// TestRebuildZeroRefsUnreferenced: a message saved to storage but referenced by
// no folder index is NOT resurrected into INBOX — it is reported and its refcount
// is recomputed to 0 so the next purge reclaims it. The referenced message keeps
// a non-zero refcount.
func TestRebuildZeroRefsUnreferenced(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "referenced message body\r\n")

	// Unreferenced: written to storage, never appended to any folder index.
	orphanFn, _, err := box.Save("INBOX", strings.NewReader("orphan body\r\n"), 0, 12, nil)
	if err != nil {
		t.Fatalf("save orphan: %v", err)
	}
	orphanUID, _ := parseFilename(orphanFn)

	stats, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.UnreferencedZeroref != 1 {
		t.Errorf("unreferenced zeroref = %d, want 1", stats.UnreferencedZeroref)
	}
	// INBOX is NOT grown — no resurrection.
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX count = %d, want 1 (unreferenced message must not be adopted)", got)
	}
	// The unreferenced record is now zero-ref (purge will reclaim it); the
	// referenced one is not.
	zeroRef := map[uint32]bool{}
	m, _ := box.openMap()
	for _, uid := range m.CompactGarbage() {
		zeroRef[uid] = true
	}
	if !zeroRef[orphanUID] {
		t.Errorf("unreferenced map_uid %d should be zero-ref after rebuild", orphanUID)
	}
	if len(zeroRef) != 1 {
		t.Errorf("zero-ref count = %d, want 1 (only the unreferenced message)", len(zeroRef))
	}
}

// TestRebuildDropsDanglingFolderRecord: a folder record pointing at a map_uid
// that storage does not have is dropped (its message is gone).
func TestRebuildDropsDanglingFolderRecord(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "keep me\r\n")

	// Dangling: index references map_uid 999999 which was never stored.
	f, _ := idx.OpenFolder("INBOX", 0)
	uid, _ := idx.AllocateUID(f.ID)
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "999999", Size: 4}); err != nil {
		t.Fatal(err)
	}
	if got := folderCount(t, idx, "INBOX"); got != 2 {
		t.Fatalf("pre-rebuild INBOX = %d, want 2", got)
	}

	stats, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("post-rebuild INBOX = %d, want 1 (dangling record dropped)", got)
	}
	_ = stats
}

// TestRebuildExpungesVanishedMapRecord: a message whose m.<N> file was deleted
// is dropped from BOTH the folder index and the map.
func TestRebuildExpungesVanishedMapRecord(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	// Small rotate size so the second message rolls into its own m.2 cheaply.
	box, idx := newBoxAndIndex(t, home, WithRotateSize(4096))
	deliverMsg(t, box, idx, "INBOX", "stays in m.1\r\n")
	// A body over the 4 KiB rotate size forces a rotation into m.2, isolating it.
	big := "X" + strings.Repeat("y", 4096+16)
	deliverMsg(t, box, idx, "INBOX", big)

	// Delete m.2 so its message vanishes from storage.
	if err := os.Remove(box.mfilePath(2)); err != nil {
		t.Fatalf("remove m.2: %v", err)
	}

	stats, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Expunged != 1 {
		t.Errorf("expunged map records = %d, want 1", stats.Expunged)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX = %d, want 1 (vanished message dropped)", got)
	}
	// Map no longer carries the vanished record.
	m, _ := box.openMap()
	if _, ok, _ := m.Lookup(2); ok {
		t.Error("map still holds the vanished map_uid 2")
	}
}

// TestRebuildBumpsGenerationCounter: the persisted rebuild_count increments per
// rebuild and survives a reopen (header migrated to 8 bytes, stably).
func TestRebuildBumpsGenerationCounter(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "body\r\n")

	s1, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := box.RebuildStorage(idx, false)
	if err != nil {
		t.Fatal(err)
	}
	if s1.RebuildCount != 1 || s2.RebuildCount != 2 {
		t.Fatalf("rebuild counts = %d,%d, want 1,2", s1.RebuildCount, s2.RebuildCount)
	}

	// Reopen the whole storage and confirm the counter persisted.
	box2, _ := newBoxAndIndex(t, home)
	m, err := box2.openMap()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.RebuildCount(); got != 2 {
		t.Errorf("reopened rebuild_count = %d, want 2", got)
	}
}

// TestRebuildAbortsOnUnmountedAlt: alt storage configured but its directory is
// absent must abort before any expunge (would otherwise mass-delete alt mail).
func TestRebuildAbortsOnUnmountedAlt(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	altHome := filepath.Join(base, "alt")
	box := New(WithAltStorage(filepath.Join(altHome, "%u"))).OpenUser(
		&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home})
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	// altStoragePath() does not exist → guard must fire.
	_, err := box.RebuildStorage(idx, false)
	if err == nil || !strings.Contains(err.Error(), "alt storage") {
		t.Fatalf("expected alt-unavailable abort, got %v", err)
	}
}

// TestRebuildAbortsOnIncompleteScan: a corrupt m.<N> makes the scan incomplete,
// and the rebuild must refuse (never expunge on an unreadable tier).
func TestRebuildAbortsOnIncompleteScan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "good record\r\n")
	// Append garbage with no LF so the next record can never be framed.
	f, err := os.OpenFile(box.mfilePath(1), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(strings.Repeat("X", 100)))
	_ = f.Close()

	_, err = box.RebuildStorage(idx, false)
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("expected ErrScanIncomplete abort, got %v", err)
	}
	// Nothing was expunged: the good message still resolves.
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX = %d, want 1 (nothing dropped on abort)", got)
	}
}
