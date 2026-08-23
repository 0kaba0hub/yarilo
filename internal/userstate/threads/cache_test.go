package threads

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Folding a sidecar is O(account) and delivery happens per message, so the
// second delivery to an account must not refold it. The far end of that claim
// is the file itself: if the cache is bypassed, the read shows up as a fold.
func TestASecondDeliveryDoesNotRefold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	c := NewCache(time.Minute)

	first, err := c.Get("u@example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(path, first, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"}); err != nil {
		t.Fatal(err)
	}
	c.Note("u@example.com", path)

	second, err := c.Get("u@example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Error("the state was refolded after this process wrote it")
	}
	if id, ok := second.ThreadOfGUID("g1"); !ok || id != "g1" {
		t.Errorf("the cached state lost the placement: %q/%v", id, ok)
	}
}

// Another process appended while we held a folded copy. Threading from the
// stale one assigns a second thread id to a conversation that already has one
// -- a split, and the kind that no later delivery repairs.
func TestAnotherWriterInvalidatesTheFold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	c := NewCache(time.Minute)

	if _, err := c.Get("u@example.com", path); err != nil {
		t.Fatal(err)
	}

	// A different process: its own state, its own append.
	theirs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Append(path, theirs, Placement{GUID: "g9", MessageID: "theirs@x", ThreadID: "g9"}); err != nil {
		t.Fatal(err)
	}
	// mtime granularity is coarse on some filesystems; the size changed, which
	// is the other half of the freshness check and the reason both are used.

	mine, err := c.Get("u@example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := mine.ThreadOfMessage("theirs@x"); !ok || id != "g9" {
		t.Errorf("the other writer's placement is invisible: %q/%v -- this state would re-thread that conversation", id, ok)
	}
}

// An unmigrated account has no file at all, and that must not turn into a fold
// per delivery either: it is the common case until the migration step has run
// everywhere.
func TestAnAbsentSidecarIsCachedToo(t *testing.T) {
	c := NewCache(time.Minute)
	path := filepath.Join(t.TempDir(), FileName)

	first, err := c.Get("u@example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Get("u@example.com", path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("an account with no sidecar is refolded on every delivery")
	}
}

// Bounded by idleness, not by process lifetime: a cache of every account ever
// delivered to holds their maps until restart, and the account nobody has
// written to in an hour is exactly the one whose memory should go back (#1396).
func TestAnIdleAccountIsDropped(t *testing.T) {
	c := NewCache(50 * time.Millisecond)
	path := filepath.Join(t.TempDir(), FileName)

	if _, err := c.Get("u@example.com", path); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Fatalf("cached = %d, want 1", c.Len())
	}
	time.Sleep(80 * time.Millisecond)
	// Any use sweeps: a cache with no traffic holds nothing worth reclaiming.
	if _, err := c.Get("other@example.com", path); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Errorf("cached = %d after the first account went idle, want 1 (the new one)", c.Len())
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("this test assumed no sidecar was written")
	}
}

// The migration step replaces the file wholesale, so a process holding a fold
// of the old one has to be told.
func TestForgetDropsAnAccount(t *testing.T) {
	c := NewCache(time.Minute)
	path := filepath.Join(t.TempDir(), FileName)
	if _, err := c.Get("u@example.com", path); err != nil {
		t.Fatal(err)
	}
	c.Forget("u@example.com")
	if c.Len() != 0 {
		t.Errorf("cached = %d after Forget, want 0", c.Len())
	}
}

// The setting says "negative = never cache, pays the fold on every delivery",
// and it has to mean that. It promised the opposite twice over before this: a
// negative idle became the default period in NewCache, and even past that,
// eviction was off while entries kept accumulating -- "never cache" would have
// read as "hold every account until restart", the leak the bound exists to
// prevent (#1396).
//
// An operator reaching for this is usually trying to give memory back on a hot
// node, or to take the cache out of a freshness question. Both are the exact
// opposite of what the old behaviour delivered.
func TestNeverCacheKeepsNothing(t *testing.T) {
	c := NewCache(-1)
	path := filepath.Join(t.TempDir(), FileName)

	for i := 0; i < 3; i++ {
		if _, err := c.Get("u@example.com", path); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Folds(); got != 3 {
		t.Errorf("folds = %d for three reads with caching off, want 3", got)
	}
	if got := c.Len(); got != 0 {
		t.Errorf("held accounts = %d with caching off, want 0", got)
	}
}

// Zero still means the built-in period, which is the other half of the
// contract and the one every deployment uses.
func TestZeroIdleSelectsTheDefault(t *testing.T) {
	c := NewCache(0)
	path := filepath.Join(t.TempDir(), FileName)
	if _, err := c.Get("u@example.com", path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("u@example.com", path); err != nil {
		t.Fatal(err)
	}
	if got := c.Folds(); got != 1 {
		t.Errorf("folds = %d with the default idle, want 1", got)
	}
	if got := c.Len(); got != 1 {
		t.Errorf("held accounts = %d, want 1", got)
	}
}
