package director

import (
	"testing"
	"time"
)

func TestUserDir_SetGet(t *testing.T) {
	d := NewUserDir(time.Minute)
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
	d := NewUserDir(time.Minute)
	d.Set("bob@example.com", "10.0.0.2:993", true)
	e := d.Get("bob@example.com")
	if e == nil || !e.Weak {
		t.Error("expected Weak=true")
	}
}

func TestUserDir_Expiry(t *testing.T) {
	d := NewUserDir(50 * time.Millisecond)
	d.Set("carol@example.com", "10.0.0.3:993", false)

	time.Sleep(100 * time.Millisecond)
	if e := d.Get("carol@example.com"); e != nil {
		t.Errorf("expected expired entry to return nil, got %+v", e)
	}
}

func TestUserDir_Delete(t *testing.T) {
	d := NewUserDir(time.Minute)
	d.Set("dave@example.com", "10.0.0.4:993", false)
	d.Delete("dave@example.com")
	if e := d.Get("dave@example.com"); e != nil {
		t.Error("expected nil after delete")
	}
}

func TestUserDir_HashConsistency(t *testing.T) {
	h1 := HashUsername("user@example.com")
	h2 := HashUsername("user@example.com")
	if h1 != h2 {
		t.Errorf("hash not deterministic: %d vs %d", h1, h2)
	}
}

func TestUserDir_SetByHash(t *testing.T) {
	d := NewUserDir(time.Minute)
	h := HashUsername("eve@example.com")
	d.SetByHash(h, "10.0.0.5:993", false)

	e := d.GetByHash(h)
	if e == nil || e.Host != "10.0.0.5:993" {
		t.Errorf("SetByHash/GetByHash mismatch: %+v", e)
	}
}

func TestUserDir_Purge(t *testing.T) {
	d := NewUserDir(50 * time.Millisecond)
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
	d := NewUserDir(time.Minute)
	d.Set("u1@x.com", "10.0.0.1:993", false)
	d.Set("u2@x.com", "10.0.0.2:993", false)

	snap := d.Snapshot()
	if len(snap) != 2 {
		t.Errorf("expected 2 snapshot entries, got %d", len(snap))
	}
}
