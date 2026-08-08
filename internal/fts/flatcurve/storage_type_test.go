//go:build flatcurve

package flatcurve

import "testing"

// The declared storage type is what decides whether the durability call is
// issued -- not a comment, and not a guess about the mount. Both directions
// pinned: dropping the gate makes the nfs row fail, hard-coding the skip
// makes the local rows fail.
func TestStorageTypeDecidesDirSync(t *testing.T) {
	cases := []struct {
		storageType string
		want        bool
	}{
		{"", true},      // unset = local = do the extra work
		{"local", true}, // fsync is what makes the rename survive a crash
		{"nfs", false},  // metadata already committed by protocol; no-op
	}
	for _, c := range cases {
		if got := (Options{StorageType: c.storageType}).dirSyncUseful(); got != c.want {
			t.Errorf("storage type %q: dirSyncUseful() = %v, want %v", c.storageType, got, c.want)
		}
	}
}

// An engine built with the nfs type still compacts correctly -- skipping the
// fsync changes durability on crash, never the result.
func TestOptimizeUnderNFSStorageTypeStillMerges(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1, OptimizeLimit: 0, StorageType: "nfs"})
	for uid := uint32(1); uid <= 6; uid++ {
		indexDoc(t, ui, uid, nil, []string{"needle"})
	}
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := ui.OptimizeMailbox(inbox); err != nil {
		t.Fatal(err)
	}
	paths, err := shardPaths(ui.(*userIndex).state(inbox).dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Errorf("%d shards after compaction, want 1 merged", len(paths))
	}
	res, err := ui.Lookup(inbox, bodyQuery("needle"))
	if err != nil || len(res.Definite) != 6 {
		t.Errorf("after compaction: %d of 6 documents (err %v)", len(res.Definite), err)
	}
}
