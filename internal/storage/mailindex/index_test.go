package mailindex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewFileEmptyHasMatchingHeader(t *testing.T) {
	exts := []Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
		{Name: "keywords", RecordSize: 4, RecordAlign: 4},
	}
	f, err := NewFile(1717185600, exts)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if f.Header.RecordSize != f.Layout.RecordSize {
		t.Errorf("Header.RecordSize=%d, Layout.RecordSize=%d", f.Header.RecordSize, f.Layout.RecordSize)
	}
	if f.Header.HeaderSize <= uint32(f.Header.BaseHeaderSize) {
		t.Errorf("Header.HeaderSize=%d, BaseHeaderSize=%d (must be greater because we have extensions)",
			f.Header.HeaderSize, f.Header.BaseHeaderSize)
	}
	if len(f.Records) != 0 {
		t.Errorf("fresh file has %d records, want 0", len(f.Records))
	}
}

func TestRecreateReadRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "dovecot.index")

	exts := []Extension{
		{Name: "modseq", HdrSize: 16, HdrData: bytes.Repeat([]byte{0xAA}, 16),
			RecordSize: 8, RecordAlign: 8, ResetID: 7},
		{Name: "keywords", RecordSize: 4, RecordAlign: 4, ResetID: 1},
	}
	f, err := NewFile(1717185600, exts)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	f.Header.UIDValidity = 1717180000
	f.Header.NextUID = 6
	f.Header.MessagesCount = 5
	f.Header.SeenMessagesCount = 3
	for uid := uint32(1); uid <= 5; uid++ {
		f.Records = append(f.Records, &Record{
			UID:   uid,
			Flags: FlagSeen,
			Ext: map[string][]byte{
				"modseq":   {byte(uid), 0, 0, 0, 0, 0, 0, 0},
				"keywords": {byte(uid), 0, 0, 0},
			},
		})
	}

	if _, err := Recreate(f.ToRecreateInput(path)); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Header.UIDValidity != f.Header.UIDValidity {
		t.Errorf("UIDValidity drift: got %d, want %d", got.Header.UIDValidity, f.Header.UIDValidity)
	}
	if got.Header.NextUID != f.Header.NextUID {
		t.Errorf("NextUID drift: got %d, want %d", got.Header.NextUID, f.Header.NextUID)
	}
	if got.Header.MessagesCount != f.Header.MessagesCount {
		t.Errorf("MessagesCount drift: got %d, want %d", got.Header.MessagesCount, f.Header.MessagesCount)
	}
	if got.Header.RecordSize != f.Header.RecordSize {
		t.Errorf("RecordSize drift: got %d, want %d", got.Header.RecordSize, f.Header.RecordSize)
	}
	if len(got.Records) != len(f.Records) {
		t.Fatalf("records count: got %d, want %d", len(got.Records), len(f.Records))
	}
	for i, want := range f.Records {
		if got.Records[i].UID != want.UID {
			t.Errorf("rec %d UID: got %d, want %d", i, got.Records[i].UID, want.UID)
		}
		if got.Records[i].Flags != want.Flags {
			t.Errorf("rec %d Flags: got 0x%02x, want 0x%02x", i, got.Records[i].Flags, want.Flags)
		}
		for name, wantBytes := range want.Ext {
			if !bytes.Equal(got.Records[i].Ext[name], wantBytes) {
				t.Errorf("rec %d ext %q drift", i, name)
			}
		}
	}
	if len(got.Extensions) != len(f.Extensions) {
		t.Errorf("extensions: got %d, want %d", len(got.Extensions), len(f.Extensions))
	}
	for _, want := range f.Extensions {
		var match *Extension
		for i := range got.Extensions {
			if got.Extensions[i].Name == want.Name {
				match = &got.Extensions[i]
				break
			}
		}
		if match == nil {
			t.Errorf("ext %q missing after round-trip", want.Name)
			continue
		}
		if match.RecordOffset != want.RecordOffset {
			t.Errorf("ext %q RecordOffset: got %d, want %d", want.Name, match.RecordOffset, want.RecordOffset)
		}
		if !bytes.Equal(match.HdrData, want.HdrData) {
			t.Errorf("ext %q HdrData drift", want.Name)
		}
	}
}

func TestRecreateKeepBackupHardlinks(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "dovecot.index")

	exts := []Extension{{Name: "modseq", RecordSize: 8, RecordAlign: 8}}
	f, _ := NewFile(1, exts)
	if _, err := Recreate(f.ToRecreateInput(path)); err != nil {
		t.Fatalf("first Recreate: %v", err)
	}
	// Second recreate with KeepBackup should hardlink the first.
	in := f.ToRecreateInput(path)
	in.KeepBackup = true
	backup, err := Recreate(in)
	if err != nil {
		t.Fatalf("second Recreate: %v", err)
	}
	if backup == "" {
		t.Fatal("KeepBackup=true returned empty backup path")
	}
	if backup != path+".backup" {
		t.Errorf("backup path %q, want %q", backup, path+".backup")
	}
	bothExist := func(paths ...string) bool {
		for _, p := range paths {
			if _, err := Open(p); err != nil {
				return false
			}
		}
		return true
	}
	if !bothExist(path, backup) {
		t.Error("either index or backup missing after second Recreate")
	}
}

func TestRecreateRejectsLayoutMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "x.index")
	f, _ := NewFile(1, []Extension{{Name: "modseq", RecordSize: 8, RecordAlign: 8}})
	in := f.ToRecreateInput(path)
	in.Header.RecordSize = 99 // doesn't match layout
	_, err := Recreate(in)
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted on RecordSize mismatch", err)
	}
}

func TestSyncLockKeyDeterminism(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "x.index")
	k1 := SyncLockKey(path)
	k2 := SyncLockKey(path)
	if k1 != k2 {
		t.Errorf("SyncLockKey not deterministic: %q vs %q", k1, k2)
	}
	other := SyncLockKey(filepath.Join(tmpDir, "y.index"))
	if k1 == other {
		t.Errorf("SyncLockKey collision across paths: %q", k1)
	}
}

func TestWithSyncLockNilLocker(t *testing.T) {
	// nil locker → no-op wrapper. The fn must run, the error
	// must propagate.
	called := false
	err := WithSyncLock(context.Background(), nil, "x", "owner", 0, func() error {
		called = true
		return errors.New("inner")
	})
	if !called {
		t.Error("fn not called when locker is nil")
	}
	if err == nil || err.Error() != "inner" {
		t.Errorf("err=%v, want inner error propagated", err)
	}
}

// Integration of WithSyncLock against a real locks.Locker is
// covered by pkg/locks/locks_test.go's suite (every backend
// must pass the Counter/Acquire/Renew/Subscribe contract). Here
// we only confirm the nil-locker fast path and the deterministic
// key derivation — anything more belongs in the consumer's tests.

func TestRecreateTmpDir(t *testing.T) {
	indexDir := t.TempDir()
	volatileDir := t.TempDir() // simulates a separate FS (different dir = different path)

	path := filepath.Join(indexDir, "test.index")
	f, err := NewFile(1, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	in := f.ToRecreateInput(path)
	in.TmpDir = volatileDir

	if _, err := Recreate(in); err != nil {
		t.Fatalf("Recreate with TmpDir: %v", err)
	}

	// Target index must exist and be readable.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index not written: %v", err)
	}

	// No leftover tmp or stage files in either dir.
	for _, dir := range []string{indexDir, volatileDir} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "test.index" && (filepath.Ext(e.Name()) == ".tmp" ||
				contains(e.Name(), ".tmp.") || contains(e.Name(), ".stage.")) {
				t.Errorf("leftover file in %s: %s", dir, e.Name())
			}
		}
	}

	// Index must be round-trip readable.
	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer fh.Close()
	if _, err := Read(fh); err != nil {
		t.Fatalf("Read after TmpDir Recreate: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAddHeaderExtension covers the backfill helper used to add a header-only
// extension to an index that lacks it (#586): HeaderSize must be fixed up so
// Recreate accepts the file and a reopen finds the extension.
func TestAddHeaderExtension(t *testing.T) {
	f, err := NewFile(1717185600, []Extension{
		{Name: "modseq", HdrSize: 16, HdrData: bytes.Repeat([]byte{0xAA}, 16),
			RecordSize: 8, RecordAlign: 8, ResetID: 7},
	})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	data := bytes.Repeat([]byte{0x42}, 16)
	if err := f.AddHeaderExtension("hdr-vsize", data, 8, 1); err != nil {
		t.Fatalf("AddHeaderExtension: %v", err)
	}

	// Header must now match the encoded extensions, else Recreate rejects it.
	path := filepath.Join(t.TempDir(), "dovecot.index")
	if _, err := Recreate(RecreateInput{
		Path: path, Header: f.Header, Extensions: f.Extensions, Records: f.Records,
	}); err != nil {
		t.Fatalf("Recreate after AddHeaderExtension (HeaderSize not fixed up?): %v", err)
	}
	ro, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var got *Extension
	for i := range ro.Extensions {
		if ro.Extensions[i].Name == "hdr-vsize" {
			got = &ro.Extensions[i]
		}
	}
	if got == nil {
		t.Fatal("hdr-vsize extension not persisted")
	}
	if !bytes.Equal(got.HdrData, data) {
		t.Errorf("HdrData = %x, want %x", got.HdrData, data)
	}

	// Idempotent: a second add is a no-op (no duplicate, no error).
	before := len(f.Extensions)
	if err := f.AddHeaderExtension("hdr-vsize", data, 8, 1); err != nil {
		t.Fatalf("second AddHeaderExtension: %v", err)
	}
	if len(f.Extensions) != before {
		t.Errorf("extension count changed on no-op add: %d -> %d", before, len(f.Extensions))
	}
}
