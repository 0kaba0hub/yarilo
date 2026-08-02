package mdbox

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// openTestUserMailbox creates a Backend + userMailbox rooted at home.
func openTestUserMailbox(t *testing.T, home string) *userMailbox {
	t.Helper()
	b := New()
	u := b.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestGUIDIndexed verifies that a saved message's GUID is stored in
// the mdboxmap and retrievable via LookupByGUID.
func TestGUIDIndexed(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	body := "From: a@a.com\r\nSubject: guid-test\r\n\r\nbody\r\n"
	filename, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}

	// Retrieve the map_uid from the filename.
	mapUID, err := parseFilename(filename)
	if err != nil {
		t.Fatal(err)
	}

	// The map entry should have a non-zero GUID.
	entry, ok, err := m.Lookup(mapUID)
	if err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}
	if entry.GUID == ([16]byte{}) {
		t.Error("map entry GUID is zero after Save")
	}

	// LookupByGUID should find the same entry.
	found, ok, err := m.LookupByGUID(entry.GUID)
	if err != nil || !ok {
		t.Fatalf("LookupByGUID: ok=%v err=%v", ok, err)
	}
	if found.UID != entry.UID {
		t.Errorf("LookupByGUID returned UID=%d, want %d", found.UID, entry.UID)
	}
}

// TestGUIDPreservedAfterPurge verifies that a compacted record keeps
// its original GUID — the GUID must survive the purge compaction path.
func TestGUIDPreservedAfterPurge(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	bodies := []string{
		"From: a@a.com\r\n\r\nfirst\r\n",
		"From: b@b.com\r\n\r\nsecond\r\n",
	}
	filenames := make([]string, len(bodies))
	for i, b := range bodies {
		fn, _, err := u.Save("INBOX", strings.NewReader(b), 0, int64(len(b)), nil)
		if err != nil {
			t.Fatalf("Save[%d]: %v", i, err)
		}
		filenames[i] = fn
	}

	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}

	// Capture GUIDs before purge.
	preGUIDs := make(map[uint32][16]byte)
	for _, fn := range filenames {
		uid, _ := parseFilename(fn)
		e, ok, err := m.Lookup(uid)
		if err != nil || !ok {
			t.Fatalf("pre-purge Lookup uid=%d: ok=%v err=%v", uid, ok, err)
		}
		preGUIDs[uid] = e.GUID
	}

	// Remove the first message so the file becomes partially dead.
	if err := u.Remove("INBOX", filenames[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Purge — compacts the file.
	if _, err := u.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	// Re-open map after purge (it was rewritten).
	u.mu.Lock()
	u.mapping = nil
	u.mu.Unlock()
	m, err = u.openMap()
	if err != nil {
		t.Fatal(err)
	}

	// The surviving message's GUID must be unchanged.
	uid1, _ := parseFilename(filenames[1])
	e, ok, err := m.Lookup(uid1)
	if err != nil || !ok {
		t.Fatalf("post-purge Lookup: ok=%v err=%v", ok, err)
	}
	if e.GUID != preGUIDs[uid1] {
		t.Errorf("GUID changed after purge: before=%x after=%x", preGUIDs[uid1], e.GUID)
	}

	// LookupByGUID still works.
	found, ok, err := m.LookupByGUID(e.GUID)
	if err != nil || !ok {
		t.Fatalf("LookupByGUID post-purge: ok=%v err=%v", ok, err)
	}
	if found.UID != uid1 {
		t.Errorf("LookupByGUID returned wrong UID: %d", found.UID)
	}
}

// TestLookupByGUID_ZeroReturnsNotFound — zero GUID must never match.
func TestLookupByGUID_ZeroReturnsNotFound(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	body := "From: a@a.com\r\n\r\nsome body\r\n"
	u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil) //nolint:errcheck

	m, _ := u.openMap()
	_, ok, err := m.LookupByGUID([16]byte{})
	if err != nil || ok {
		t.Errorf("LookupByGUID(zero) must return notFound: ok=%v err=%v", ok, err)
	}
}

// TestGUIDInRebuildScan verifies that the GUID from the dbox trailer
// is surfaced in the ScanRecord returned by scanStorage so the rebuild
// resolveMapFilenames can use it for GUID-based matching.
func TestGUIDInRebuildScan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)

	body := "From: a@a.com\r\n\r\nrebuild body\r\n"
	filename, _, _ := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil)

	recs, err := u.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("Scan returned no records")
	}

	// The scan record must carry a non-zero GUID.
	rec := recs[0]
	if rec.GUID == ([16]byte{}) {
		t.Error("ScanRecord.GUID is zero — trailer was not parsed")
	}

	// The GUID must match what the map stores.
	m, _ := u.openMap()
	uid, _ := parseFilename(filename)
	entry, ok, _ := m.Lookup(uid)
	if !ok {
		t.Fatal("map Lookup failed")
	}
	if rec.GUID != entry.GUID {
		t.Errorf("ScanRecord.GUID=%x != map GUID=%x", rec.GUID, entry.GUID)
	}

	// fetchedBody must still be readable.
	rc, err := u.Fetch("INBOX", filename, false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !strings.Contains(string(got), "rebuild body") {
		t.Errorf("unexpected body: %q", got)
	}
}
