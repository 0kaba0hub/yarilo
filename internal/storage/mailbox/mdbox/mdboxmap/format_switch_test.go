package mdboxmap

import (
	"os"
	"path/filepath"
	"testing"
)

func baseMagicOnDisk(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, MapIndexFileName))
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if len(raw) >= 5 && string(raw[0:4]) == baseMagic {
		return string(raw[0:4])
	}
	return "v1"
}

// The format on disk decides what an older binary can still open, so it is the
// operator's call, not the code's. Both directions must carry every map_uid:
// the map is the only place one exists, and a folder index that references a
// map_uid the map no longer has points at nothing.
func TestMapFormatConvertsInBothDirections(t *testing.T) {
	dir := t.TempDir()
	want := v1Entries()
	seedV1(t, dir, want, 8, 2, 5)

	assertAll := func(t *testing.T, m *Map, where string) {
		t.Helper()
		if got := m.MessageCount(); got != len(want) {
			t.Fatalf("%s: map holds %d records, want %d", where, got, len(want))
		}
		for _, w := range want {
			got, ok, err := m.Lookup(w.UID)
			if err != nil || !ok {
				t.Fatalf("%s: map_uid %d lost: ok=%v err=%v", where, w.UID, ok, err)
			}
			if got != w {
				t.Errorf("%s: map_uid %d is %+v, want %+v", where, w.UID, got, w)
			}
		}
		if m.NextMapUID() != 8 {
			t.Errorf("%s: NextMapUID = %d, want 8", where, m.NextMapUID())
		}
		if m.HighestFileID() != 2 {
			t.Errorf("%s: HighestFileID = %d, want 2", where, m.HighestFileID())
		}
		if m.RebuildCount() != 5 {
			t.Errorf("%s: RebuildCount = %d, want 5", where, m.RebuildCount())
		}
	}

	up, err := Open(dir, "alice@example.com", WithFormat(FormatV2))
	if err != nil {
		t.Fatalf("open as v2: %v", err)
	}
	assertAll(t, up, "after v1 to v2")
	if got := baseMagicOnDisk(t, dir); got != baseMagic {
		t.Fatalf("base on disk is %q after selecting v2", got)
	}
	_ = up.Close()

	// The rollback leg: the same setting pointed the other way puts the map
	// back into a format an older binary can open.
	down, err := Open(dir, "alice@example.com", WithFormat(FormatV1))
	if err != nil {
		t.Fatalf("open as v1: %v", err)
	}
	assertAll(t, down, "after v2 to v1")
	if got := baseMagicOnDisk(t, dir); got != "v1" {
		t.Fatalf("base on disk is %q after selecting v1", got)
	}
	_ = down.Close()

	// And forward again, so the pair is proven to round-trip rather than to
	// work once in each direction from a fresh file.
	back, err := Open(dir, "alice@example.com", WithFormat(FormatV2))
	if err != nil {
		t.Fatalf("reopen as v2: %v", err)
	}
	defer back.Close() //nolint:errcheck
	assertAll(t, back, "after the round trip")
}

// Selecting a format leaves the map usable in it, log and all: a delivery
// written under v1 must still be there after the next open.
func TestWritesUnderTheSelectedFormatSurvive(t *testing.T) {
	for _, format := range []Format{FormatV1, FormatV2} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			m, err := Open(dir, "alice@example.com", WithFormat(format))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			uid, err := m.AppendRecord(1, 0, 10, [16]byte{1})
			if err != nil {
				t.Fatalf("AppendRecord: %v", err)
			}
			if err := m.UpdateRefcounts([]uint32{uid}, 2); err != nil {
				t.Fatalf("UpdateRefcounts: %v", err)
			}
			_ = m.Close()

			again, err := Open(dir, "alice@example.com", WithFormat(format))
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer again.Close() //nolint:errcheck
			e, ok, err := again.Lookup(uid)
			if err != nil || !ok {
				t.Fatalf("Lookup: ok=%v err=%v", ok, err)
			}
			if e.RefCount != 3 {
				t.Errorf("refcount %d after reopen, want 3", e.RefCount)
			}
		})
	}
}

// A value nobody implements is a wiring mistake, and it names how the bytes that
// locate every message are written. Falling back to a default would turn a typo
// in values.yaml into a silent format change.
func TestUnknownMapFormatIsRefused(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com", WithFormat(Format("v3")))
	if err == nil {
		_ = m.Close()
		t.Fatal("an unknown mdbox_map_format was accepted")
	}
	if _, serr := os.Stat(filepath.Join(dir, MapIndexFileName)); !os.IsNotExist(serr) {
		t.Error("a refused format still created a map file")
	}
}

// An empty value is not a choice, it is the absence of one: it must land on the
// default rather than on a format named by the empty string.
func TestEmptyMapFormatUsesTheDefault(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com", WithFormat(Format("")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck
	if got := baseMagicOnDisk(t, dir); got != baseMagic {
		t.Errorf("base on disk is %q, want the default format", got)
	}
}
