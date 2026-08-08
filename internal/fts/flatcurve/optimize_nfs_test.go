//go:build flatcurve

package flatcurve

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// The NFS invariant optimizeDir depends on, pinned: a search never holds a
// shard open across the compaction that deletes it. Today that holds because
// every path opens and closes within the call and all of them take u.mu, so
// searches and compactions strictly alternate. Cache open readers to speed
// up search -- the obvious optimisation -- and the deletions become
// unlink-under-open: on NFS a silly-rename, a directory that will not rmdir,
// and ESTALE for the reader (#1176). This test is what such a change has to
// walk past: every lookup, before and after and during compaction, must
// return the document, and no .nfs* or leftover tmp may remain.
func TestOptimizeRacesLookupAndLeavesNoOpenShards(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1, OptimizeLimit: 0})
	for uid := uint32(1); uid <= 12; uid++ {
		indexDoc(t, ui, uid, []string{"subj"}, []string{"needle", "hay"})
	}
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}

	dir := ui.(*userIndex).state(inbox).dir
	if paths, err := shardPaths(dir); err != nil || len(paths) < 2 {
		t.Fatalf("need at least 2 shards to compact, got %v (err %v)", paths, err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	// Readers hammer the index while compactions run underneath them.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 60; i++ {
				res, err := ui.Lookup(inbox, bodyQuery("needle"))
				if err != nil {
					t.Errorf("lookup during compaction: %v", err)
					return
				}
				if len(res.Definite) != 12 {
					t.Errorf("lookup during compaction saw %d of 12 documents", len(res.Definite))
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 6; i++ {
			if err := ui.OptimizeMailbox(inbox); err != nil {
				t.Errorf("optimize: %v", err)
				return
			}
			indexDoc(t, ui, uint32(100+i), []string{"subj"}, []string{"filler"})
		}
	}()
	close(start)
	wg.Wait()

	// Nothing deleted while open, nothing half-renamed: no silly-rename
	// stubs and no compaction tmp left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nfs") {
			t.Errorf("silly-rename stub left behind: %s", e.Name())
		}
		if e.Name() == "optimize" {
			t.Errorf("compaction tmp dir left behind: %s", e.Name())
		}
	}
	res, err := ui.Lookup(inbox, bodyQuery("needle"))
	if err != nil || len(res.Definite) != 12 {
		t.Errorf("after compaction: %d of 12 documents (err %v)", len(res.Definite), err)
	}
}

// Whole-user optimize compacts every open mailbox -- the behaviour kept when
// the entry point moved to a per-mailbox loop under each mailbox's own lock
// (#1176). Asserted by shard count, since that is what compaction changes.
func TestMailboxesDrivesWholeUserOptimize(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1, OptimizeLimit: 0})
	other := fts.MailboxRef{GUID: "g2", Name: "Sent", UIDValidity: 1}
	for uid := uint32(1); uid <= 6; uid++ {
		indexDoc(t, ui, uid, nil, []string{"alpha"})
		indexDocIn(t, ui, other, uid, nil, []string{"beta"})
	}
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}

	boxes := ui.Mailboxes()
	if len(boxes) != 2 {
		t.Fatalf("Mailboxes() = %v, want both open mailboxes", boxes)
	}
	// The service loops exactly like this, taking each mailbox's lock.
	for _, mbox := range boxes {
		if err := ui.OptimizeMailbox(mbox); err != nil {
			t.Fatal(err)
		}
	}
	for _, mbox := range []fts.MailboxRef{inbox, other} {
		paths, err := shardPaths(ui.(*userIndex).state(mbox).dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 {
			t.Errorf("%s: %d shards after whole-user optimize, want 1 merged", mbox.Name, len(paths))
		}
	}
}
