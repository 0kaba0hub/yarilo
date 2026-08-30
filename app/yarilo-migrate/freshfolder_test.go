package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// A folder written but not yet flushed to a base index goes down the index
// branch, not the store scan. Its whole state is in its log, and the scan
// delivers messages with no flags and no keywords -- on a freshly created store
// that is every flag in the account (#1564).
//
// The oracle is the reference's own fetch over the folder this fixture came
// from: uid 1 \Seen, uid 2 \Answered, uid 3 $Important.
func TestAFolderWithOnlyALogIsReadThroughTheIndexBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.IndexFreshLog(t), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, exts, err := readReferenceFolder(dir)
	if err != nil {
		t.Fatalf("read folder: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d messages, and the reference reports three", len(recs))
	}
	got := map[uint32][]string{}
	for _, r := range recs {
		got[r.UID] = append(flagNames(r.Flags), r.Keywords...)
	}
	for uid, want := range map[uint32]string{1: `\Seen`, 2: `\Answered`, 3: "$Important"} {
		if len(got[uid]) != 1 || got[uid][0] != want {
			t.Errorf("uid %d reads as %v, and the reference reports %s", uid, got[uid], want)
		}
	}
	if len(exts) == 0 {
		t.Error("no extensions came back; a message's bytes are found through one")
	}
}

// Neither file: still the scan, and still not an empty folder.
func TestAFolderWithNeitherIndexNorLogGoesToTheScan(t *testing.T) {
	if _, _, err := readReferenceFolder(t.TempDir()); err == nil {
		t.Fatal("a folder with no index and no log read clean")
	}
}
