package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/dicttest"
)

func TestContractSuite(t *testing.T) {
	dicttest.RunSuite(t, func(t *testing.T) dict.Dict {
		path := filepath.Join(t.TempDir(), "dict.json")
		d, err := New(dict.Config{Settings: map[string]any{"path": path}})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return d
	})
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.json")

	d1, _ := New(dict.Config{Settings: map[string]any{"path": path}})
	tx, _ := d1.Begin(context.Background(), nil)
	_ = tx.Set("priv/box/INBOX/comment", []byte("seen by next process"))
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	d1.Close() //nolint:errcheck

	// Simulate restart: brand-new Dict instance over the same path.
	d2, _ := New(dict.Config{Settings: map[string]any{"path": path}})
	defer d2.Close() //nolint:errcheck

	vals, found, err := d2.Lookup(context.Background(), nil, "priv/box/INBOX/comment")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found || string(vals[0]) != "seen by next process" {
		t.Fatalf("post-reopen lookup: found=%v values=%q", found, vals)
	}
}

func TestAtomicRenameLeavesNoPartialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rename.json")

	d, _ := New(dict.Config{Settings: map[string]any{"path": path}})
	defer d.Close() //nolint:errcheck

	tx, _ := d.Begin(context.Background(), nil)
	for i := 0; i < 50; i++ {
		_ = tx.Set("key/"+string(rune('a'+i%26))+string(rune('0'+i%10)), []byte("value"))
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) {
			continue
		}
		t.Errorf("unexpected leftover file after commit: %s", e.Name())
	}
}

func TestMissingPathSettingErrors(t *testing.T) {
	if _, err := New(dict.Config{}); err == nil {
		t.Error("missing path should error")
	}
	if _, err := New(dict.Config{Settings: map[string]any{"path": ""}}); err == nil {
		t.Error("empty path should error")
	}
	if _, err := New(dict.Config{Settings: map[string]any{"path": 123}}); err == nil {
		t.Error("non-string path should error")
	}
}

func TestRegisteredAtInit(t *testing.T) {
	for _, n := range dict.Drivers() {
		if n == "file" {
			return
		}
	}
	t.Errorf("file driver not registered: %v", dict.Drivers())
}

func TestExpireScanPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expire.json")
	d, _ := New(dict.Config{Settings: map[string]any{"path": path}})
	defer d.Close() //nolint:errcheck

	// Write a row whose TTL is already in the past by writing a row,
	// then editing the file directly to set Expires=1.
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Set("stale", []byte("v"))
	_ = tx.Set("fresh", []byte("v"))
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Force "stale" to expire by overwriting the file with a hand-
	// rolled envelope containing Expires=1 (a unix time in 1970).
	raw := `{"version":1,"entries":[
        {"k":"stale","v":["dg=="],"exp":1},
        {"k":"fresh","v":["dg=="]}
    ]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	d.Close() //nolint:errcheck

	d2, _ := New(dict.Config{Settings: map[string]any{"path": path}})
	defer d2.Close() //nolint:errcheck
	if err := d2.ExpireScan(context.Background()); err != nil {
		t.Fatalf("expire-scan: %v", err)
	}
	if _, found, _ := d2.Lookup(context.Background(), nil, "stale"); found {
		t.Error("expired row still present after ExpireScan")
	}
	if _, found, _ := d2.Lookup(context.Background(), nil, "fresh"); !found {
		t.Error("ExpireScan dropped a non-TTL row")
	}
}
