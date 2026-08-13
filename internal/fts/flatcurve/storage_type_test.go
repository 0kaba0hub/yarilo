//go:build flatcurve

package flatcurve

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/ftsstore"
)

// An engine built with the nfs type still compacts correctly -- skipping the
// fsync changes durability on crash, never the result.
func TestOptimizeUnderNFSStorageTypeStillMerges(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1, OptimizeLimit: 0,
		Store: ftsstore.NewPosix(Layout(), ftsstore.StorageTypeNFS)})
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
