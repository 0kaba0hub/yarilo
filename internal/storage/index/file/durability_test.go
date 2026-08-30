package file

import (
	"os"
	"strings"
	"testing"
)

// The conversion asks for a durable flush, and the removal comes after it.
//
// A source check, and deliberately so. What has to hold is an ordering between
// three calls -- the base rewrite is told to fsync, the directory entry is
// synced, their index is unlinked -- and the states it protects against only
// appear when the machine stops between two of them. Everything reachable from
// a running test is downstream of the ordering being right; this is upstream of
// it.
//
// The flush is not durable anywhere else in this package, because everywhere
// else the state still exists somewhere when a tail is lost. Here it does not:
// the other copy is deleted a few lines later (#1524).
func TestTheConversionOrdersDurabilityBeforeRemoval(t *testing.T) {
	src, err := os.ReadFile("foreign.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	steps := []string{
		"fs.fsyncOnFlush = true",
		"fs.flush(true)",
		"fsyncDir(fs.indexDir)",
		"dboxconv.RemoveForeignFolder(dir)",
	}
	at := -1
	for _, step := range steps {
		i := strings.Index(body, step)
		if i < 0 {
			t.Fatalf("the conversion no longer contains %q", step)
		}
		if i < at {
			t.Errorf("%q happens before the step above it; ours has to be on the disk before theirs is unlinked", step)
		}
		at = i
	}

	// And the flag has to be honoured where the base is written.
	folder, err := os.ReadFile("folder.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(folder), "ri.Fsync = fs.fsyncOnFlush") {
		t.Error("the base rewrite no longer takes its durability from the folder state")
	}
}
