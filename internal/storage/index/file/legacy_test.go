package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// writeLegacyIndex synthesises a pre-Phase-2 yarilo-format .index
// file at path with the supplied state. Mirrors what the old
// fileindex would have written so the legacy decoder + adoption
// path can be exercised end-to-end without depending on the
// old code remaining in tree.
func writeLegacyIndex(t *testing.T, path string, indexID, uidValidity, nextUID uint32, modseq uint64, guid [16]byte, records []legacyTestRec) {
	t.Helper()
	buf := make([]byte, 120)
	le := binary.LittleEndian
	buf[0] = 7
	buf[1] = legacyMinor
	le.PutUint16(buf[2:], 120) // base_header_size
	le.PutUint32(buf[4:], 120) // header_size == base (no real ext region)
	le.PutUint32(buf[8:], legacyRecordSize)
	buf[12] = 0x01 // compat_flags LE
	le.PutUint32(buf[16:], indexID)
	le.PutUint32(buf[24:], uidValidity)
	le.PutUint32(buf[28:], nextUID)
	le.PutUint32(buf[32:], uint32(len(records)))
	le.PutUint64(buf[legacyHdrOffMagic:], modseq)
	copy(buf[legacyHdrOffGUID:legacyHdrOffGUID+16], guid[:])

	recBuf := make([]byte, legacyRecordSize*len(records))
	for i, r := range records {
		off := i * legacyRecordSize
		le.PutUint32(recBuf[off:], r.uid)
		recBuf[off+4] = r.flags
		le.PutUint64(recBuf[off+5:], r.modseq)
		le.PutUint32(recBuf[off+13:], r.keywordBits)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	out := append(buf, recBuf...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

type legacyTestRec struct {
	uid         uint32
	flags       uint8
	modseq      uint64
	keywordBits uint32
}

func TestAutoMigratesLegacyOnOpen(t *testing.T) {
	dir := t.TempDir()
	// Seed under the legacy filename — Open() must first rename
	// dovecot.index → yarilo.index, then notice the pre-Phase-2
	// yarilo wire format inside and migrate to current format.
	indexPath := filepath.Join(dir, "dovecot.index")

	guid := [16]byte{0xab, 0xcd, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05,
		0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d}
	writeLegacyIndex(t, indexPath, 1717185600, 1717180000, 4,
		17 /* highest modseq */, guid,
		[]legacyTestRec{
			{uid: 1, flags: flagSeen, modseq: 5},
			{uid: 2, flags: flagSeen | flagFlagged, modseq: 10},
			{uid: 3, flags: flagDeleted, modseq: 17},
		})

	be := New()
	idx := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: dir})
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	if folder.UIDValidity != 1717180000 {
		t.Errorf("UIDValidity drift: got %d, want 1717180000", folder.UIDValidity)
	}
	if folder.NextUID != 4 {
		t.Errorf("NextUID drift: got %d, want 4", folder.NextUID)
	}
	if folder.Messages != 3 {
		t.Errorf("Messages drift: got %d, want 3", folder.Messages)
	}
	if folder.HighestModSeq != 17 {
		t.Errorf("HighestModSeq drift: got %d, want 17", folder.HighestModSeq)
	}
	if folder.GUID != guid {
		t.Errorf("GUID drift: got %x, want %x", folder.GUID, guid)
	}

	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d msgs after migration, want 3", len(msgs))
	}
	wantFlags := map[uint32][]string{
		1: {`\Seen`},
		2: {`\Flagged`, `\Seen`},
		3: {`\Deleted`},
	}
	for _, m := range msgs {
		wantSlice := wantFlags[m.UID]
		if len(m.Flags) != len(wantSlice) {
			t.Errorf("UID %d: got flags %v, want %v", m.UID, m.Flags, wantSlice)
			continue
		}
		gotSet := map[string]bool{}
		for _, f := range m.Flags {
			gotSet[f] = true
		}
		for _, w := range wantSlice {
			if !gotSet[w] {
				t.Errorf("UID %d: missing flag %s in %v", m.UID, w, m.Flags)
			}
		}
	}

	// After Open the legacy-named file is gone (renamed in) and
	// the .legacy backup of the pre-Phase-2 wire format sits next
	// to the yarilo-native index file.
	yariloPath := filepath.Join(dir, IndexFileName)
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Errorf("legacy-named index file still present: %v", err)
	}
	if _, err := os.Stat(yariloPath + ".legacy"); err != nil {
		t.Errorf("expected %s.legacy backup, got: %v", yariloPath, err)
	}

	// And the .index file on disk should now be canonical-format.
	// Detect by re-running detector — it must say "not legacy".
	if _, isLegacy, _ := detectAndDecodeLegacy(yariloPath); isLegacy {
		t.Error("post-migration file still detected as legacy")
	}
}

// TestOpenMigratesDovecotIndexFilename covers the pure-filename
// migration path: a current-format file under the legacy name
// gets renamed in place; no wire-format conversion happens.
func TestOpenMigratesDovecotIndexFilename(t *testing.T) {
	dir := t.TempDir()
	be := New()
	idx := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: dir})

	// 1. Open under the yarilo name to seed a normal index.
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	yariloPath := filepath.Join(dir, IndexFileName)
	yariloLog := filepath.Join(dir, IndexLogFileName)
	if _, err := os.Stat(yariloPath); err != nil {
		t.Fatalf("seed index missing: %v", err)
	}

	// 2. Move the yarilo files back to legacy names — as if the
	//    folder was inherited from a canonical install.
	legacyPath := filepath.Join(dir, LegacyIndexFileName)
	legacyLog := filepath.Join(dir, LegacyIndexLogFileName)
	if err := os.Rename(yariloPath, legacyPath); err != nil {
		t.Fatalf("rename to legacy: %v", err)
	}
	if err := os.Rename(yariloLog, legacyLog); err != nil {
		t.Fatalf("rename log to legacy: %v", err)
	}

	// 3. Fresh OpenUser → must surface the renamed files.
	// Close the first handle so the cache entry is evicted and the second
	// OpenUser gets a brand-new userIndex that discovers the legacy names.
	idx.Close() //nolint:errcheck
	idx2 := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: dir})
	if _, err := idx2.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("Open after legacy seed: %v", err)
	}
	if _, err := os.Stat(yariloPath); err != nil {
		t.Errorf("yarilo-native index not present after migration: %v", err)
	}
	if _, err := os.Stat(yariloLog); err != nil {
		t.Errorf("yarilo-native log not present after migration: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy index file still on disk: %v", err)
	}
	if _, err := os.Stat(legacyLog); !os.IsNotExist(err) {
		t.Errorf("legacy log file still on disk: %v", err)
	}
}
