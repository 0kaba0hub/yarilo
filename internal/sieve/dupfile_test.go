package sieve

import (
	"context"
	"testing"
)

func TestFileDuplicateTracker(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	tr := NewFileDuplicateTracker(home, "", nil) // nil locker = no cross-process lock (test)

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

	pod1 := NewFileDuplicateTracker(home, "", nil)
	pod2 := NewFileDuplicateTracker(home, "", nil)

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
	tr := NewFileDuplicateTracker(home, "", nil)

	// TTL 0 → immediately expired; the next check must not see it as a duplicate.
	if dup, _ := tr.IsDuplicate(ctx, "h", "id", 0, false); dup {
		t.Fatal("first with ttl 0 must be new")
	}
	if dup, _ := tr.IsDuplicate(ctx, "h", "id", 3600, false); dup {
		t.Fatal("expired entry must not count as duplicate")
	}
}
