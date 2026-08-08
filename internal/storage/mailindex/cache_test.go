package mailindex

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestCache(t *testing.T) (*CacheFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), CacheFileName)
	c, err := CreateCache(path, 42, 1000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, path
}

func TestCacheOffsetPacking(t *testing.T) {
	// The packed form has the high bit of every byte set, so a torn write
	// can never decode as a valid offset -- the property the format buys.
	for _, off := range []uint32{0, 4, 8, 1024, 0x001fc000, 0x0ffffffc, 0x3ffffffc} {
		packed := packCacheOffset(off)
		if got := unpackCacheOffset(packed); got != off {
			t.Errorf("pack/unpack(%#x) = %#x", off, got)
		}
	}
	if got := unpackCacheOffset(0x00001234); got != 0 {
		t.Errorf("garbage without marker bits decoded to %#x, want 0", got)
	}
}

func TestCacheCreateOpenRoundTrip(t *testing.T) {
	c, path := newTestCache(t)
	if _, err := c.AddFields([]CacheField{
		{Name: "envelope", Type: CacheFieldVariableSize, Decision: CacheDecisionTemp},
		{Name: "date.received", Type: CacheFieldFixedSize, Decision: CacheDecisionYes, Size: 4},
	}); err != nil {
		t.Fatal(err)
	}
	id, ok := c.FieldID("envelope")
	if !ok {
		t.Fatal("envelope field lost")
	}
	off, err := c.AppendRecord(0, []CacheFieldValue{
		{FieldID: id, Data: []byte("subject: odd-length")}, // 19 bytes: exercises padding
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	r, err := OpenCache(path, 42, 1000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r.Close()
	if len(r.Fields()) != 2 || r.Fields()[1].Size != 4 {
		t.Fatalf("fields after reopen: %+v", r.Fields())
	}
	vals, err := r.ReadRecord(off)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vals[id], []byte("subject: odd-length")) {
		t.Errorf("record data = %q", vals[id])
	}
}

// Every validation failure is ErrCacheInvalid -- treated as absent, never
// surfaced: the cache has no vote on its own validity.
func TestCacheOpenRejections(t *testing.T) {
	c, path := newTestCache(t)
	c.Close()

	cases := []struct {
		name    string
		mutate  func(b []byte)
		indexID uint32
		fileSeq uint32
	}{
		{"wrong indexid", nil, 43, 1000},
		{"wrong file_seq (purged elsewhere)", nil, 42, 1001},
		{"wrong major", func(b []byte) { b[0] = 9 }, 42, 1000},
		{"wrong sizeof(uoff_t)", func(b []byte) { b[1] = 4 }, 42, 1000},
		{"producer generation moved", func(b []byte) { b[3] = CacheProducerGen + 1 }, 42, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), CacheFileName)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(b)
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenCache(p, tc.indexID, tc.fileSeq); !errors.Is(err, ErrCacheInvalid) {
				t.Errorf("want ErrCacheInvalid, got %v", err)
			}
		})
	}
}

// A second AddFields links a new table from the old one; reopen follows the
// chain to the newest -- ids stay stable across the append.
func TestCacheFieldTableChain(t *testing.T) {
	c, path := newTestCache(t)
	if _, err := c.AddFields([]CacheField{{Name: "a", Type: CacheFieldString, Decision: CacheDecisionYes}}); err != nil {
		t.Fatal(err)
	}
	firstNew, err := c.AddFields([]CacheField{
		{Name: "a", Type: CacheFieldString, Decision: CacheDecisionYes}, // dup: not re-added
		{Name: "b", Type: CacheFieldFixedSize, Decision: CacheDecisionYes, Size: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstNew != 1 {
		t.Fatalf("firstNew = %d, want 1", firstNew)
	}
	c.Close()

	r, err := OpenCache(path, 42, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := len(r.Fields()); got != 2 {
		t.Fatalf("fields = %d, want 2 (dup must not double)", got)
	}
	if id, _ := r.FieldID("a"); id != 0 {
		t.Errorf("field a moved to id %d", id)
	}
	if id, _ := r.FieldID("b"); id != 1 {
		t.Errorf("field b at id %d", id)
	}
}

// prev_offset chains merge newest-first: the record pointed at from the
// index wins for a field both carry.
func TestCacheRecordChainNewestWins(t *testing.T) {
	c, _ := newTestCache(t)
	if _, err := c.AddFields([]CacheField{
		{Name: "x", Type: CacheFieldVariableSize, Decision: CacheDecisionYes},
		{Name: "y", Type: CacheFieldVariableSize, Decision: CacheDecisionYes},
	}); err != nil {
		t.Fatal(err)
	}
	old, err := c.AppendRecord(0, []CacheFieldValue{
		{FieldID: 0, Data: []byte("stale")},
		{FieldID: 1, Data: []byte("only-here")},
	})
	if err != nil {
		t.Fatal(err)
	}
	newest, err := c.AppendRecord(old, []CacheFieldValue{{FieldID: 0, Data: []byte("fresh")}})
	if err != nil {
		t.Fatal(err)
	}
	vals, err := c.ReadRecord(newest)
	if err != nil {
		t.Fatal(err)
	}
	if string(vals[0]) != "fresh" || string(vals[1]) != "only-here" {
		t.Errorf("merged chain = %q / %q", vals[0], vals[1])
	}
	if c.Header().ContinuedRecordCount != 1 || c.Header().RecordCount != 2 {
		t.Errorf("counters: %+v", c.Header())
	}
}

func TestCacheFixedSizeMismatchRefused(t *testing.T) {
	c, _ := newTestCache(t)
	if _, err := c.AddFields([]CacheField{{Name: "f", Type: CacheFieldFixedSize, Decision: CacheDecisionYes, Size: 4}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppendRecord(0, []CacheFieldValue{{FieldID: 0, Data: []byte("12345")}}); err == nil {
		t.Error("5 bytes into a 4-byte fixed field accepted")
	}
}
