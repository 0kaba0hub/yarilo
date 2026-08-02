package quota

import (
	"context"
	"strconv"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/fail"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
)

func newMem(t *testing.T) dict.Dict {
	t.Helper()
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("memory dict: %v", err)
	}
	return d
}

func readInt(t *testing.T, d dict.Dict, user, key string) int64 {
	t.Helper()
	vs, found, err := d.Lookup(context.Background(), &dict.OpSettings{Username: user}, key)
	if err != nil || !found || len(vs) == 0 {
		t.Fatalf("lookup %s: found=%v err=%v", key, found, err)
	}
	n, _ := strconv.ParseInt(string(vs[0]), 10, 64)
	return n
}

func TestClone_FanOut(t *testing.T) {
	d1, d2 := newMem(t), newMem(t)
	c := NewClone([]dict.Dict{d1, d2})
	c.Write(context.Background(), "u@x", Usage{StorageBytes: 4096, Messages: 7})

	for _, d := range []dict.Dict{d1, d2} {
		if got := readInt(t, d, "u@x", KeyStorage); got != 4096 {
			t.Errorf("storage = %d, want 4096", got)
		}
		if got := readInt(t, d, "u@x", KeyMessages); got != 7 {
			t.Errorf("messages = %d, want 7", got)
		}
	}
}

func TestClone_BestEffortPerTarget(t *testing.T) {
	// A failing target must not prevent the healthy one from being written.
	good := newMem(t)
	bad, err := fail.New(dict.Config{Driver: "fail"})
	if err != nil {
		t.Fatalf("fail dict: %v", err)
	}
	c := NewClone([]dict.Dict{bad, good})
	c.Write(context.Background(), "u@x", Usage{StorageBytes: 100, Messages: 1})

	if got := readInt(t, good, "u@x", KeyStorage); got != 100 {
		t.Errorf("healthy target storage = %d, want 100", got)
	}
}

func TestClone_NilDisabled(t *testing.T) {
	if NewClone(nil) != nil {
		t.Error("no targets should yield a nil (disabled) clone")
	}
	var c *Clone
	c.Write(context.Background(), "u@x", Usage{}) // must not panic
}
