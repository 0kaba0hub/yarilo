//go:build flatcurve

package flatcurve

import "testing"

// TestReopenAfterDiscard verifies the #629 recovery contract: after the write
// shard is released the way every engine error path now does (discardCurrent),
// the next update reopens the existing shard and keeps indexing — instead of the
// old behaviour where a poisoned st.cur was returned forever. The previously
// committed document must survive and the new one must be searchable.
func TestReopenAfterDiscard(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"alpha"})

	// Simulate what an engine error triggers: drop the open write handle.
	u := ui.(*userIndex)
	u.mu.Lock()
	u.state(inbox).discardCurrent()
	u.mu.Unlock()

	// The next update must reopen the shard, not fail on a dead handle.
	indexDoc(t, ui, 2, nil, []string{"bravo"})

	for _, tc := range []struct {
		word string
		uid  uint32
	}{{"alpha", 1}, {"bravo", 2}} {
		res, err := ui.Lookup(inbox, bodyQuery(tc.word))
		if err != nil {
			t.Fatalf("lookup %q: %v", tc.word, err)
		}
		got := append(res.Definite, res.Maybe...)
		if len(got) != 1 || got[0] != tc.uid {
			t.Errorf("lookup %q = %v, want [%d]", tc.word, got, tc.uid)
		}
	}
}
