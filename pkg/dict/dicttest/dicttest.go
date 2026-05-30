// Package dicttest is a contract test suite that any pkg/dict driver
// can run against. Each driver test does:
//
//	func TestFoo(t *testing.T) {
//	    dicttest.RunSuite(t, func(t *testing.T) dict.Dict {
//	        // return a fresh empty dict
//	    })
//	}
//
// Keeping the suite here means new drivers gain coverage for free, and
// drift between drivers (e.g. one driver silently misordering IterSortByKey
// results) gets caught the moment its package builds.
package dicttest

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Factory returns a fresh empty Dict for a single test. Called once
// per subtest. The Dict is Closed automatically via t.Cleanup.
type Factory func(t *testing.T) dict.Dict

// RunSuite runs every contract test against the driver returned by
// factory. Tests are sub-tested so a driver failure flags the exact
// behaviour at fault.
func RunSuite(t *testing.T, factory Factory) {
	t.Helper()
	tests := map[string]func(*testing.T, dict.Dict){
		"LookupMissingReturnsNotFound":     testLookupMissing,
		"SetThenLookup":                    testSetThenLookup,
		"OverwriteValue":                   testOverwriteValue,
		"UnsetRemovesKey":                  testUnsetRemovesKey,
		"UnsetMissingIsNoOp":               testUnsetMissingIsNoOp,
		"RollbackDiscardsBuffer":           testRollbackDiscards,
		"AtomicIncOnMissingKey":            testAtomicIncMissing,
		"AtomicIncIncrementsAndDecrements": testAtomicInc,
		"IteratePrefixShallow":             testIteratePrefixShallow,
		"IteratePrefixRecurse":             testIteratePrefixRecurse,
		"IterateExactKey":                  testIterateExactKey,
		"IterateNoValueSkipsValues":        testIterateNoValue,
		"IterateSortByKey":                 testIterateSortByKey,
		"IterateEmptyPathRecurse":          testIterateEmptyRecurse,
		"PrivateAndSharedNamespaces":       testNamespaces,
		"TxAfterCommitErrors":              testTxAfterCommit,
		"ContextCancellationOnLookup":      testContextCancel,
		"ExpireScanRunsCleanly":            testExpireScanClean,
		"CloseThenLookupReturnsClosed":     testCloseThenLookup,
	}
	names := make([]string, 0, len(tests))
	for n := range tests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fn := tests[name]
		t.Run(name, func(t *testing.T) {
			d := factory(t)
			t.Cleanup(func() { _ = d.Close() })
			fn(t, d)
		})
	}
}

func mustCommit(t *testing.T, tx dict.Tx) {
	t.Helper()
	res, err := tx.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res != dict.CommitOK {
		t.Fatalf("commit result = %v, want CommitOK", res)
	}
}

func mustSet(t *testing.T, d dict.Dict, key string, val []byte) {
	t.Helper()
	tx, err := d.Begin(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.Set(key, val); err != nil {
		t.Fatalf("set: %v", err)
	}
	mustCommit(t, tx)
}

func testLookupMissing(t *testing.T, d dict.Dict) {
	vals, found, err := d.Lookup(context.Background(), nil, "priv/no/such/key")
	if err != nil {
		t.Fatalf("lookup missing key: err=%v", err)
	}
	if found {
		t.Fatalf("found=true for missing key (values=%v)", vals)
	}
	if vals != nil {
		t.Fatalf("values=%v, want nil", vals)
	}
}

func testSetThenLookup(t *testing.T, d dict.Dict) {
	mustSet(t, d, "priv/box/INBOX/comment", []byte("hello"))
	vals, found, err := d.Lookup(context.Background(), nil, "priv/box/INBOX/comment")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("found=false after set")
	}
	if len(vals) != 1 || string(vals[0]) != "hello" {
		t.Fatalf("values=%q, want [hello]", vals)
	}
}

func testOverwriteValue(t *testing.T, d dict.Dict) {
	mustSet(t, d, "k", []byte("v1"))
	mustSet(t, d, "k", []byte("v2"))
	vals, _, _ := d.Lookup(context.Background(), nil, "k")
	if len(vals) != 1 || string(vals[0]) != "v2" {
		t.Fatalf("overwrite: got %q, want [v2]", vals)
	}
}

func testUnsetRemovesKey(t *testing.T, d dict.Dict) {
	mustSet(t, d, "k", []byte("v"))
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Unset("k")
	mustCommit(t, tx)
	_, found, _ := d.Lookup(context.Background(), nil, "k")
	if found {
		t.Fatal("key persisted after unset")
	}
}

func testUnsetMissingIsNoOp(t *testing.T, d dict.Dict) {
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Unset("never-existed")
	mustCommit(t, tx)
}

func testRollbackDiscards(t *testing.T, d dict.Dict) {
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Set("rb", []byte("buffered"))
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	_, found, _ := d.Lookup(context.Background(), nil, "rb")
	if found {
		t.Fatal("rolled-back key still visible")
	}
}

func testAtomicIncMissing(t *testing.T, d dict.Dict) {
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.AtomicInc("counter", 1)
	res, err := tx.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res != dict.CommitNotFound {
		t.Fatalf("atomic-inc on missing key: result=%v, want CommitNotFound", res)
	}
}

func testAtomicInc(t *testing.T, d dict.Dict) {
	mustSet(t, d, "counter", []byte("10"))
	for _, delta := range []int64{5, -3, 8} {
		tx, _ := d.Begin(context.Background(), nil)
		_ = tx.AtomicInc("counter", delta)
		mustCommit(t, tx)
	}
	vals, _, _ := d.Lookup(context.Background(), nil, "counter")
	n, _ := strconv.ParseInt(string(vals[0]), 10, 64)
	if n != 10+5-3+8 {
		t.Fatalf("counter=%d, want %d", n, 10+5-3+8)
	}
}

func seedTree(t *testing.T, d dict.Dict) {
	t.Helper()
	seed := map[string]string{
		"priv/box/INBOX/comment":      "inbox-comment",
		"priv/box/INBOX/vendor/extra": "inbox-vendor",
		"priv/box/Trash/comment":      "trash-comment",
		"shared/box/INBOX/admin":      "shared-admin",
	}
	tx, _ := d.Begin(context.Background(), nil)
	for k, v := range seed {
		_ = tx.Set(k, []byte(v))
	}
	mustCommit(t, tx)
}

func collect(t *testing.T, it dict.Iterator) map[string]string {
	t.Helper()
	got := map[string]string{}
	for it.Next() {
		vs := it.Values()
		if len(vs) > 0 {
			got[it.Key()] = string(vs[0])
		} else {
			got[it.Key()] = ""
		}
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("iter close: %v", err)
	}
	return got
}

func testIteratePrefixShallow(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, err := d.Iterate(context.Background(), nil, "priv/box/INBOX/", 0)
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	got := collect(t, it)
	if _, ok := got["priv/box/INBOX/comment"]; !ok {
		t.Errorf("missing direct child priv/box/INBOX/comment; got=%v", got)
	}
	if _, ok := got["priv/box/INBOX/vendor/extra"]; ok {
		t.Errorf("shallow iterate must not include nested vendor/extra; got=%v", got)
	}
}

func testIteratePrefixRecurse(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, _ := d.Iterate(context.Background(), nil, "priv/box/INBOX/", dict.IterRecurse)
	got := collect(t, it)
	want := []string{"priv/box/INBOX/comment", "priv/box/INBOX/vendor/extra"}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("recurse: missing %q; got=%v", k, got)
		}
	}
	if _, ok := got["priv/box/Trash/comment"]; ok {
		t.Errorf("recurse must not cross sibling Trash; got=%v", got)
	}
}

func testIterateExactKey(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, _ := d.Iterate(context.Background(), nil, "priv/box/INBOX/comment", dict.IterExactKey)
	got := collect(t, it)
	if len(got) != 1 {
		t.Fatalf("exact-key returned %d rows, want 1: %v", len(got), got)
	}
	if got["priv/box/INBOX/comment"] != "inbox-comment" {
		t.Errorf("exact-key value mismatch: %v", got)
	}
}

func testIterateNoValue(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, _ := d.Iterate(context.Background(), nil, "priv/box/INBOX/", dict.IterNoValue)
	defer it.Close()
	for it.Next() {
		if v := it.Values(); v != nil {
			t.Errorf("IterNoValue: got values=%v, want nil", v)
		}
	}
}

func testIterateSortByKey(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, _ := d.Iterate(context.Background(), nil, "priv/", dict.IterRecurse|dict.IterSortByKey)
	defer it.Close()
	var keys []string
	for it.Next() {
		keys = append(keys, it.Key())
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Errorf("IterSortByKey not sorted: %v", keys)
			break
		}
	}
}

func testIterateEmptyRecurse(t *testing.T, d dict.Dict) {
	seedTree(t, d)
	it, _ := d.Iterate(context.Background(), nil, "", dict.IterRecurse)
	got := collect(t, it)
	if len(got) != 4 {
		t.Errorf("empty-path recurse returned %d rows, want 4: %v", len(got), got)
	}
}

func testNamespaces(t *testing.T, d dict.Dict) {
	mustSet(t, d, dict.PathPrivate+"alice/x", []byte("priv-x"))
	mustSet(t, d, dict.PathShared+"alice/x", []byte("shared-x"))
	priv, _, _ := d.Lookup(context.Background(), nil, dict.PathPrivate+"alice/x")
	shared, _, _ := d.Lookup(context.Background(), nil, dict.PathShared+"alice/x")
	if string(priv[0]) != "priv-x" || string(shared[0]) != "shared-x" {
		t.Errorf("namespace bleed: priv=%q shared=%q", priv[0], shared[0])
	}
}

func testTxAfterCommit(t *testing.T, d dict.Dict) {
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Set("k", []byte("v"))
	mustCommit(t, tx)
	if err := tx.Set("k2", []byte("late")); err == nil {
		t.Error("Set after Commit should error")
	}
}

func testContextCancel(t *testing.T, d dict.Dict) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := d.Lookup(ctx, nil, "k")
	if err == nil {
		t.Error("lookup with cancelled context should return error")
	}
}

func testExpireScanClean(t *testing.T, d dict.Dict) {
	if err := d.ExpireScan(context.Background()); err != nil {
		t.Errorf("expire-scan on empty dict: %v", err)
	}
	mustSet(t, d, "k", []byte("v"))
	if err := d.ExpireScan(context.Background()); err != nil {
		t.Errorf("expire-scan with rows: %v", err)
	}
	// Key without explicit TTL must survive expire-scan.
	_, found, _ := d.Lookup(context.Background(), nil, "k")
	if !found {
		t.Error("non-TTL key removed by expire-scan")
	}
}

func testCloseThenLookup(t *testing.T, d dict.Dict) {
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, _, err := d.Lookup(context.Background(), nil, "k")
	if err == nil {
		t.Error("lookup after close should error")
	}
}

// suppress unused import warning when this file is the only one in pkg.
var _ = time.Second
