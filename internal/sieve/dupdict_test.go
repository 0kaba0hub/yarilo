package sieve

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
)

func newMemDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatalf("memory dict: %v", err)
	}
	return d
}

func TestDictDuplicateTracker(t *testing.T) {
	ctx := context.Background()
	d := newMemDict(t)
	tr := NewDictDuplicateTracker(d, "u1@x")

	// First sighting → not a duplicate.
	if dup, err := tr.IsDuplicate(ctx, "h", "msg-id-1", 3600, false); err != nil || dup {
		t.Fatalf("first IsDuplicate = (%v,%v), want (false,nil)", dup, err)
	}
	// Same id again → duplicate.
	if dup, err := tr.IsDuplicate(ctx, "h", "msg-id-1", 3600, false); err != nil || !dup {
		t.Fatalf("second IsDuplicate = (%v,%v), want (true,nil)", dup, err)
	}
	// Different id → not a duplicate.
	if dup, _ := tr.IsDuplicate(ctx, "h", "msg-id-2", 3600, false); dup {
		t.Fatal("different id must not be a duplicate")
	}
	// Different handle, same id → separate namespace, not a duplicate.
	if dup, _ := tr.IsDuplicate(ctx, "other", "msg-id-1", 3600, false); dup {
		t.Fatal("different handle must not collide")
	}
}

func TestDictDuplicateTracker_CrossPod(t *testing.T) {
	ctx := context.Background()
	shared := newMemDict(t) // one shared (e.g. redis) dict = two pods

	pod1 := NewDictDuplicateTracker(shared, "u1@x")
	pod2 := NewDictDuplicateTracker(shared, "u1@x")

	if dup, _ := pod1.IsDuplicate(ctx, "h", "id", 3600, false); dup {
		t.Fatal("pod1 first delivery must not be a duplicate")
	}
	// A different pod sharing the dict sees the entry.
	if dup, _ := pod2.IsDuplicate(ctx, "h", "id", 3600, false); !dup {
		t.Fatal("pod2 must observe pod1's entry via the shared dict (cross-pod dedup)")
	}
}
