package sieve

import (
	"context"
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestFileDuplicateTracker(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	tr := NewFileDuplicateTracker("u1@example.com", home, "", nil) // nil locker = no cross-process lock (test)

	if dup, err := tr.IsDuplicate(ctx, "h", "id-1", 3600, false); err != nil || dup {
		t.Fatalf("first = (%v,%v), want (false,nil)", dup, err)
	}
	if dup, _ := tr.IsDuplicate(ctx, "h", "id-1", 3600, false); !dup {
		t.Fatal("repeat must be a duplicate")
	}
	if dup, _ := tr.IsDuplicate(ctx, "h", "id-2", 3600, false); dup {
		t.Fatal("different id must not be a duplicate")
	}
	if dup, _ := tr.IsDuplicate(ctx, "other", "id-1", 3600, false); dup {
		t.Fatal("different handle must not collide")
	}
}

func TestFileDuplicateTracker_CrossPod(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir() // shared home dir on shared storage = two pods

	pod1 := NewFileDuplicateTracker("u1@example.com", home, "", nil)
	pod2 := NewFileDuplicateTracker("u1@example.com", home, "", nil)

	if dup, _ := pod1.IsDuplicate(ctx, "h", "id", 3600, false); dup {
		t.Fatal("pod1 first delivery must not be a duplicate")
	}
	if dup, _ := pod2.IsDuplicate(ctx, "h", "id", 3600, false); !dup {
		t.Fatal("pod2 must observe pod1's entry via the shared home file")
	}
}

func TestFileDuplicateTracker_Expiry(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	tr := NewFileDuplicateTracker("u1@example.com", home, "", nil)

	// TTL 0 → immediately expired; the next check must not see it as a duplicate.
	if dup, _ := tr.IsDuplicate(ctx, "h", "id", 0, false); dup {
		t.Fatal("first with ttl 0 must be new")
	}
	if dup, _ := tr.IsDuplicate(ctx, "h", "id", 3600, false); dup {
		t.Fatal("expired entry must not count as duplicate")
	}
}

func TestDupTracker_DriverSelection(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		driver string
		want   string // type name fragment
	}{
		{"file", "*sieve.FileDuplicateTracker"},
		{"memory", "*interp.MemoryDuplicateTracker"},
		{"", "*sieve.FileDuplicateTracker"}, // empty defaults to file
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			e := &Engine{cfg: config.SieveConfig{DuplicateDriver: tc.driver}}
			tr := e.dupTrackerBackend("u1", home)
			if got := typeName(tr); got != tc.want {
				t.Errorf("driver %q → %s, want %s", tc.driver, got, tc.want)
			}
		})
	}
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}

type recordingTracker struct{ gotSeconds uint32 }

func (r *recordingTracker) IsDuplicate(_ context.Context, _, _ string, seconds uint32, _ bool) (bool, error) {
	r.gotSeconds = seconds
	return false, nil
}

func TestClampedDuplicateTracker(t *testing.T) {
	rec := &recordingTracker{}
	c := clampedDuplicateTracker{inner: rec, maxSeconds: 604800} // 7 days

	// over the cap → clamped
	_, _ = c.IsDuplicate(context.Background(), "h", "id", 999999999, false)
	if rec.gotSeconds != 604800 {
		t.Errorf("clamp: inner got %d, want 604800", rec.gotSeconds)
	}
	// under the cap → unchanged
	_, _ = c.IsDuplicate(context.Background(), "h", "id", 3600, false)
	if rec.gotSeconds != 3600 {
		t.Errorf("under cap: inner got %d, want 3600", rec.gotSeconds)
	}
	// cap 0 → no limit
	c0 := clampedDuplicateTracker{inner: rec, maxSeconds: 0}
	_, _ = c0.IsDuplicate(context.Background(), "h", "id", 999999999, false)
	if rec.gotSeconds != 999999999 {
		t.Errorf("no cap: inner got %d, want 999999999", rec.gotSeconds)
	}
}
