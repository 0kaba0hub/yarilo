package mdboxmap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// Another implementation's map is not taken into our namespace on the strength
// of its name.
//
// The legacy name is ours historically and theirs currently. Renaming theirs
// destroys the file they look for before anything has decided this store should
// be touched, and what lands under our name is then read as a map of ours: wrong
// offsets, and a conversion that stops partway leaving both halves broken
// (#1590).
func TestAForeignMapBaseIsNotClaimedByItsName(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "dovecot.map.index")
	if err := os.WriteFile(legacy, dboxref.MapBase(t), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := mdboxmap.Open(dir, "u1@example.com")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close() //nolint:errcheck

	// Theirs is still where they left it.
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("their map base was taken away: %v", err)
	}
	// And ours is a fresh, empty map -- not their bytes read as ours.
	if n := len(m.Records()); n != 0 {
		t.Errorf("our map opened with %d records, and nothing of ours has been written yet", n)
	}
}

// A map of ours under the legacy name is still adopted: the migration this
// guard narrows is a real one that stores in the field still need.
func TestOurOwnMapUnderTheLegacyNameIsStillMigrated(t *testing.T) {
	dir := t.TempDir()

	// Write one of ours, in the v1 format that legacy name goes with.
	seed, err := mdboxmap.Open(dir, "u1@example.com", mdboxmap.WithFormat(mdboxmap.FormatV1))
	if err != nil {
		t.Fatal(err)
	}
	var none [16]byte
	uid, err := seed.AppendRecord(1, 16, 100, none)
	if err != nil {
		t.Fatal(err)
	}
	// Stamping a guid rewrites the base, so the record is in the file this test
	// is about rather than only in the log beside it.
	guid := [16]byte{7}
	if _, err := seed.SetGUIDs(map[uint32][16]byte{uid: guid}); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	native := filepath.Join(dir, "yarilo.map.index")
	legacy := filepath.Join(dir, "dovecot.map.index")
	if err := os.Rename(native, legacy); err != nil {
		t.Fatal(err)
	}
	// The log carries the same records; move it aside so the base is what is
	// read, which is the case this guard sits in.
	_ = os.Remove(filepath.Join(dir, "yarilo.map.index.log"))

	m, err := mdboxmap.Open(dir, "u1@example.com", mdboxmap.WithFormat(mdboxmap.FormatV1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close() //nolint:errcheck
	recs := m.Records()
	if len(recs) != 1 {
		t.Fatalf("our own legacy-named map came back with %d records, want 1", len(recs))
	}
	if recs[0].GUID != guid {
		t.Errorf("record guid is %x, want %x", recs[0].GUID, guid)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the legacy name is still there after migrating ours: %v", err)
	}
}
