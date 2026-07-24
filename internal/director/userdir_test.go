package director

import (
	"sync"
	"testing"
	"time"
)

func TestUserDir_SetGet(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	d.Set("alice@example.com", "10.0.0.1:993", false)

	e := d.Get("alice@example.com")
	if e == nil {
		t.Fatal("expected entry, got nil")
	}
	if e.Host != "10.0.0.1:993" {
		t.Errorf("host: want 10.0.0.1:993, got %q", e.Host)
	}
	if e.Weak {
		t.Error("want Weak=false")
	}
}

func TestUserDir_WeakFlag(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	d.Set("bob@example.com", "10.0.0.2:993", true)
	e := d.Get("bob@example.com")
	if e == nil || !e.Weak {
		t.Error("expected Weak=true")
	}
}

func TestUserDir_Expiry(t *testing.T) {
	d := NewUserDir(50*time.Millisecond, true, "10.0.0.1:9102")
	d.Set("carol@example.com", "10.0.0.3:993", false)

	time.Sleep(100 * time.Millisecond)
	if e := d.Get("carol@example.com"); e != nil {
		t.Errorf("expected expired entry to return nil, got %+v", e)
	}
}

func TestUserDir_Delete(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	d.Set("dave@example.com", "10.0.0.4:993", false)
	d.Delete("dave@example.com")
	if e := d.Get("dave@example.com"); e != nil {
		t.Error("expected nil after delete")
	}
}

func TestUserDir_HashConsistency(t *testing.T) {
	h1 := HashUsername("user@example.com", true)
	h2 := HashUsername("user@example.com", true)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %d vs %d", h1, h2)
	}
}

// TestUserDir_HashLowercase proves #738: HashUsername with lowercase=true
// gives the same hash for any spelling of a username, and with
// lowercase=false the two spellings hash differently (reproducing the bug).
func TestUserDir_HashLowercase(t *testing.T) {
	if HashUsername("User@d.test", true) != HashUsername("user@d.test", true) {
		t.Error("lowercase=true: hashes for different spellings must match")
	}
	if HashUsername("User@d.test", false) == HashUsername("user@d.test", false) {
		t.Error("lowercase=false: hashes for different spellings must NOT match (this reproduces the pre-#738 bug)")
	}
}

// TestUserDir_GetSetDelete_CaseInsensitive proves a user stored under one
// spelling is retrievable and deletable under another, with lowercase=true.
func TestUserDir_GetSetDelete_CaseInsensitive(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	d.Set("User@d.test", "10.0.0.9:993", false)

	e := d.Get("user@d.test")
	if e == nil || e.Host != "10.0.0.9:993" {
		t.Fatalf("Get with different spelling: want entry, got %+v", e)
	}

	d.Delete("USER@D.TEST")
	if e := d.Get("User@d.test"); e != nil {
		t.Error("expected nil after delete via a third spelling")
	}
}

func TestUserDir_SetByHash(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	h := HashUsername("eve@example.com", true)
	d.SetByHash(h, "10.0.0.5:993", false)

	e := d.GetByHash(h)
	if e == nil || e.Host != "10.0.0.5:993" {
		t.Errorf("SetByHash/GetByHash mismatch: %+v", e)
	}
}

func TestUserDir_Purge(t *testing.T) {
	d := NewUserDir(50*time.Millisecond, true, "10.0.0.1:9102")
	d.Set("a@x.com", "10.0.0.1:993", false)
	d.Set("b@x.com", "10.0.0.2:993", false)

	time.Sleep(100 * time.Millisecond)
	d.Purge()

	snap := d.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty after purge, got %d entries", len(snap))
	}
}

func TestUserDir_Snapshot(t *testing.T) {
	d := NewUserDir(time.Minute, true, "10.0.0.1:9102")
	d.Set("u1@x.com", "10.0.0.1:993", false)
	d.Set("u2@x.com", "10.0.0.2:993", false)

	snap := d.Snapshot()
	if len(snap) != 2 {
		t.Errorf("expected 2 snapshot entries, got %d", len(snap))
	}
}

func TestUserDir_MergeByHash_Ordering(t *testing.T) {
	h := HashUsername("u@x", true)
	tests := []struct {
		name       string
		seedSeq    uint64
		seedBy     string
		seedHost   string
		inSeq      uint64
		inBy       string
		inHost     string
		wantHost   string
		wantChange bool // returned "changed backend" signal
	}{
		{"newer seq wins", 1, "a", "b1", 2, "b", "b2", "b2", true},
		{"older seq loses", 5, "a", "b1", 3, "b", "b2", "b1", false},
		{"equal seq lower id wins", 4, "z", "b1", 4, "a", "b2", "b2", true},
		{"equal seq higher id loses", 4, "a", "b1", 4, "z", "b2", "b1", false},
		{"equal seq same id same host no-op", 4, "a", "b1", 4, "a", "b1", "b1", false},
		{"newer seq same host: change=false", 1, "a", "b1", 2, "a", "b1", "b1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewUserDir(time.Minute, true, "self:9102")
			d.MergeByHash(h, tc.seedHost, false, tc.seedSeq, tc.seedBy)
			changed := d.MergeByHash(h, tc.inHost, false, tc.inSeq, tc.inBy)
			if e := d.GetByHash(h); e == nil || e.Host != tc.wantHost {
				t.Fatalf("host = %v, want %s", e, tc.wantHost)
			}
			if changed != tc.wantChange {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChange)
			}
		})
	}
}

func TestUserDir_LamportAdvancesPastRemote(t *testing.T) {
	d := NewUserDir(time.Minute, true, "self:9102")
	h := HashUsername("u@x", true)
	// A remote assignment at seq 100 must push a subsequent LOCAL assignment
	// past it, so local wins deterministically (Lamport causality).
	d.MergeByHash(h, "remote:993", false, 100, "peer:9102")
	d.Set("u@x", "local:993", false)
	if e := d.GetByHash(h); e == nil || e.Host != "local:993" || e.AssignSeq <= 100 {
		t.Fatalf("local assignment must sort after remote seq 100, got %+v", e)
	}
}

func TestUserDir_SetByHash_MonotonicUnderConcurrency(t *testing.T) {
	d := NewUserDir(time.Minute, true, "self:9102")
	h := HashUsername("hot@x", true)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.SetByHash(h, "b:993", false) }()
	}
	wg.Wait()
	// After n concurrent Sets on the same hash the persisted seq must be the
	// highest ticked value — never regressed by a later-locking, earlier-
	// ticked writer.
	if e := d.GetByHash(h); e == nil || e.AssignSeq != uint64(n) {
		t.Fatalf("persisted seq = %v, want %d (monotonic per hash)", e, n)
	}
}
