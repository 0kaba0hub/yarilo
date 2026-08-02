package memory

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/dicttest"
)

func TestContractSuite(t *testing.T) {
	dicttest.RunSuite(t, func(t *testing.T) dict.Dict {
		d, err := New(dict.Config{Driver: "memory"})
		if err != nil {
			t.Fatalf("new memory dict: %v", err)
		}
		return d
	})
}

func TestTTLExpiry(t *testing.T) {
	d, _ := New(dict.Config{Driver: "memory"})
	defer d.Close()

	tx, _ := d.Begin(context.Background(), &dict.OpSettings{ExpireSecs: 1})
	_ = tx.Set("ephemeral", []byte("byebye"))
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Force the clock past the TTL by waiting just over a second; this
	// is the only test that needs wall-clock sleep — the suite covers
	// non-TTL semantics.
	time.Sleep(1100 * time.Millisecond)

	_, found, err := d.Lookup(context.Background(), nil, "ephemeral")
	if err != nil {
		t.Fatalf("lookup post-TTL: %v", err)
	}
	if found {
		t.Error("TTL-expired key still visible")
	}
}

func TestExpireScanPurges(t *testing.T) {
	d, _ := New(dict.Config{Driver: "memory"})
	defer d.Close()

	tx, _ := d.Begin(context.Background(), &dict.OpSettings{ExpireSecs: 1})
	_ = tx.Set("k1", []byte("v"))
	_, _ = tx.Commit()

	time.Sleep(1100 * time.Millisecond)
	if err := d.ExpireScan(context.Background()); err != nil {
		t.Fatalf("expire-scan: %v", err)
	}
	// After ExpireScan, the key is gone from the internal map (verified
	// by Lookup also being false; the difference vs lazy expiry is
	// memory reclamation, which we cannot assert without internals).
	_, found, _ := d.Lookup(context.Background(), nil, "k1")
	if found {
		t.Error("expired key survived ExpireScan")
	}
}

func TestRegisteredAtInit(t *testing.T) {
	found := false
	for _, n := range dict.Drivers() {
		if n == "memory" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("memory driver not registered; got %v", dict.Drivers())
	}
}
