package uidvalidity_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/uidvalidity"
)

func newAlloc(t *testing.T) (*uidvalidity.Allocator, string) {
	t.Helper()
	dir := t.TempDir()
	return uidvalidity.New(dir, "u1@example.com", "owner", nil), dir
}

// The clock is not an allocator. Two folders created inside one second ask with
// the same stamp, and a folder recreated in the same second as its own delete
// asks with the stamp its predecessor used (#1614).
func TestTheSameStampNeverComesBackTwice(t *testing.T) {
	a, _ := newAlloc(t)
	const stamp = uint32(1788252508)
	seen := map[uint32]bool{}
	for i := 0; i < 5; i++ {
		v, err := a.Next(stamp)
		if err != nil {
			t.Fatal(err)
		}
		if seen[v] {
			t.Fatalf("value %d handed out twice from the same stamp", v)
		}
		seen[v] = true
	}
	if !seen[stamp] {
		t.Errorf("the first call did not take the stamp itself: %v", seen)
	}
}

// A clock that steps backwards asks with a number already issued.
func TestAStampBelowWhatWasIssuedLoses(t *testing.T) {
	a, _ := newAlloc(t)
	high, err := a.Next(1788252508)
	if err != nil {
		t.Fatal(err)
	}
	low, err := a.Next(1000)
	if err != nil {
		t.Fatal(err)
	}
	if low <= high {
		t.Errorf("after %d a backwards clock got %d; the counter must win", high, low)
	}
}

// The marker is the cross-process claim, so it must be on disk for every value
// handed out -- a lock cannot cover a deployment with no locker wired.
func TestEveryValueLeavesItsClaimOnDisk(t *testing.T) {
	a, dir := newAlloc(t)
	v, err := a.Next(1788252508)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, fmt.Sprintf("%s.%08x", uidvalidity.FileName, v))
	if _, serr := os.Stat(marker); serr != nil {
		t.Errorf("no claim on disk for %d: %v", v, serr)
	}
	body, err := os.ReadFile(filepath.Join(dir, uidvalidity.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%08x", v); string(body) != got {
		t.Errorf("counter holds %q, want %q", body, got)
	}
}

// Two allocators over one directory, with no lock between them, still cannot
// take one number: the filesystem decides.
func TestConcurrentAllocatorsNeverShareAValue(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	out := make([]uint32, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := uidvalidity.New(dir, "u1@example.com", "owner", nil)
			v, err := a.Next(1788252508)
			if err != nil {
				t.Errorf("allocate: %v", err)
				return
			}
			out[i] = v
		}(i)
	}
	wg.Wait()
	seen := map[uint32]int{}
	for i, v := range out {
		if v == 0 {
			t.Fatalf("allocation %d produced nothing", i)
		}
		seen[v]++
	}
	if len(seen) != n {
		t.Fatalf("%d allocations produced %d distinct values: %v", n, len(seen), seen)
	}
}

// A store written by the other implementation carries the same counter under
// its own name. It is taken over, never reseeded: reseeding from the clock
// could hand back a value that store had already issued.
func TestTheirCounterIsAdoptedRatherThanReseeded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, uidvalidity.LegacyFileName), []byte("7fffffff"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := uidvalidity.New(dir, "u1@example.com", "owner", nil)
	v, err := a.Next(1788252508) // a stamp well below theirs
	if err != nil {
		t.Fatal(err)
	}
	if v != 0x7fffffff+1 {
		t.Errorf("got %d; their counter said 0x7fffffff, so the next value must be past it", v)
	}
	if _, serr := os.Stat(filepath.Join(dir, uidvalidity.LegacyFileName)); !os.IsNotExist(serr) {
		t.Errorf("their file is still there: %v", serr)
	}
}
