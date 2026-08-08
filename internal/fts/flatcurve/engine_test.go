//go:build flatcurve

package flatcurve

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0kaba0hub/go-xapian"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func testEngine(t *testing.T, opts Options) (fts.UserIndex, fts.UserRef) {
	t.Helper()
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := New(opts).OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	return ui, user
}

var inbox = fts.MailboxRef{GUID: "g1", Name: "INBOX", UIDValidity: 1}

// indexDoc feeds one message's tokens (subject header + body words) into
// inbox. indexDocIn is the general form for tests that need a second
// mailbox (#715's OptimizeMailbox isolation test).
func indexDoc(t *testing.T, ui fts.UserIndex, uid uint32, subject []string, body []string) {
	t.Helper()
	indexDocIn(t, ui, inbox, uid, subject, body)
}

func indexDocIn(t *testing.T, ui fts.UserIndex, mbox fts.MailboxRef, uid uint32, subject []string, body []string) {
	t.Helper()
	up, err := ui.BeginUpdate(mbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(subject) > 0 {
		ok, err := up.SetBuildKey(fts.BuildKey{UID: uid, Type: fts.KeyHeader, HdrName: "subject"})
		if err != nil || !ok {
			t.Fatalf("subject key: ok=%v err=%v", ok, err)
		}
		for _, tok := range subject {
			if err := up.BuildMore([]byte(tok)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ok, err := up.SetBuildKey(fts.BuildKey{UID: uid, Type: fts.KeyBodyPart, ContentType: "text/plain"})
	if err != nil || !ok {
		t.Fatalf("body key: ok=%v err=%v", ok, err)
	}
	for _, tok := range body {
		if err := up.BuildMore([]byte(tok)); err != nil {
			t.Fatal(err)
		}
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}
}

func bodyQuery(words ...string) fts.Query {
	var ws []fts.Word
	for _, w := range words {
		ws = append(ws, fts.Word{Variants: []string{w}})
	}
	return fts.Query{Terms: []fts.Term{{Field: fts.FieldBody, Words: ws}}, AndTerms: true}
}

func TestIndexAndLookup(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, []string{"quarterly", "report"}, []string{"budget", "review"})
	indexDoc(t, ui, 2, []string{"lunch"}, []string{"budget", "pizza"})

	tests := []struct {
		name     string
		q        fts.Query
		definite []uint32
		maybe    []uint32
	}{
		{"body single word", bodyQuery("budget"), []uint32{1, 2}, nil},
		{"body AND words", bodyQuery("budget", "pizza"), []uint32{2}, nil},
		{"body prefix wildcard", bodyQuery("bud"), []uint32{1, 2}, nil},
		{"body no match", bodyQuery("zzz"), nil, nil},
		{
			"indexed header field",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "subject",
				Words: []fts.Word{{Variants: []string{"lunch"}}}}}, AndTerms: true},
			[]uint32{2}, nil,
		},
		{
			"text matches header and body",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldText,
				Words: []fts.Word{{Variants: []string{"quarterly"}}}}}, AndTerms: true},
			[]uint32{1}, nil,
		},
		{
			"non-indexed header is maybe",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "x-custom",
				Words: []fts.Word{{Variants: []string{"quarterly"}}}}}, AndTerms: true},
			nil, []uint32{1},
		},
		{
			"variants OR within word",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldBody,
				Words: []fts.Word{{Variants: []string{"zzz", "pizza"}}}}}, AndTerms: true},
			[]uint32{2}, nil,
		},
		{
			"NOT excludes",
			fts.Query{Terms: []fts.Term{
				{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"budget"}}}},
				{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"pizza"}}}, Not: true},
			}, AndTerms: true},
			[]uint32{1}, nil,
		},
		{
			"header existence probe",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "subject"}}, AndTerms: true},
			[]uint32{1, 2}, nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ui.Lookup(inbox, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Definite, tc.definite) || !reflect.DeepEqual(res.Maybe, tc.maybe) {
				t.Fatalf("definite=%v maybe=%v, want %v / %v",
					res.Definite, res.Maybe, tc.definite, tc.maybe)
			}
		})
	}
}

func TestUppercaseFirstCharHack(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	// The indexer lowercases a leading ASCII capital so it is not mistaken
	// for a Xapian prefix; the query side must apply the same rule.
	indexDoc(t, ui, 1, nil, []string{"Zebra"})
	res, err := ui.Lookup(inbox, bodyQuery("Zebra"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Definite, []uint32{1}) {
		t.Fatalf("capitalized term lookup = %v", res.Definite)
	}
}

func TestExpunge(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDoc(t, ui, 2, nil, []string{"alpha"})
	if err := ui.Expunge(inbox, 1); err != nil {
		t.Fatal(err)
	}
	res, err := ui.Lookup(inbox, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Definite, []uint32{2}) {
		t.Fatalf("after expunge = %v, want [2]", res.Definite)
	}
	// Expunging a missing UID is a no-op.
	if err := ui.Expunge(inbox, 99); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpoint(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 0 || uidv != 0 || sum != 0 {
		t.Fatalf("empty checkpoint = %d/%d/%d/%v", last, uidv, sum, err)
	}
	if err := ui.SetCheckpoint(inbox, 42, 99, 7); err != nil {
		t.Fatal(err)
	}
	last, uidv, sum, err = ui.Checkpoint(inbox)
	if err != nil || last != 42 || uidv != 99 || sum != 7 {
		t.Fatalf("checkpoint = %d/%d/%d/%v, want 42/99/7", last, uidv, sum, err)
	}
}

// TestCheckpointLegacyV1 verifies a v1 checkpoint file ("1 <uid> <sum>") still
// reads back, with uidvalidity 0 so a UIDVALIDITY mismatch resets it (#638).
func TestCheckpointLegacyV1(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	dir := (ui.(*userIndex)).state(inbox).dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointFile), []byte("1 10 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 10 || uidv != 0 || sum != 7 {
		t.Fatalf("legacy v1 checkpoint = %d/%d/%d/%v, want 10/0/7", last, uidv, sum, err)
	}
}

func TestCheckpointMigrationFallback(t *testing.T) {
	// A migrated index has no yarilo checkpoint file: last UID must
	// come from Xapian's lastdocid.
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 17, nil, []string{"legacy"})
	dir := ui.(*userIndex).state(inbox).dir
	if err := os.Remove(filepath.Join(dir, checkpointFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 17 || uidv != 0 || sum != 0 {
		t.Fatalf("migration checkpoint = %d/%d/%d/%v, want 17/0/0", last, uidv, sum, err)
	}
}

func TestRescanTargeted(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	for uid := uint32(1); uid <= 5; uid++ {
		indexDoc(t, ui, uid, nil, []string{"word"})
	}
	// Mailbox now holds 2,3,5,7: 1 and 4 were expunged offline; 7 is new.
	missing, err := ui.Rescan(inbox, []uint32{2, 3, 5, 7})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []uint32{7}) {
		t.Fatalf("missing = %v, want [7]", missing)
	}
	res, err := ui.Lookup(inbox, bodyQuery("word"))
	if err != nil {
		t.Fatal(err)
	}
	// Stale 1 and 4 removed; 2,3,5 intact (no delete-above-gap storm).
	if !reflect.DeepEqual(res.Definite, []uint32{2, 3, 5}) {
		t.Fatalf("after rescan = %v, want [2 3 5]", res.Definite)
	}
}

// TestRotateTimeTriggersRotationOnSlowCommit (#724) proves commitCurrent
// rotates when a commit exceeds RotateTime, independent of RotateCount. A
// tiny RotateTime (1ns) makes ANY real commit exceed it deterministically —
// no fake clock or artificial sleep needed, no flakiness from actual timing.
// RotateCount is set far out of reach so only the time-based trigger could
// possibly cause the rotation seen here.
func TestRotateTimeTriggersRotationOnSlowCommit(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1000, CommitLimit: 1, RotateTime: time.Nanosecond})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	dir := ui.(*userIndex).state(inbox).dir
	sealed, current := countShards(t, dir)
	if sealed < 1 {
		t.Fatalf("expected a time-based rotation after the commit: sealed=%d current=%d", sealed, current)
	}
}

// TestRotateTimeZeroDisablesTimeBasedRotation (#724) proves RotateTime: 0
// truly disables the time-based trigger, rather than withDefaults()
// silently coercing it back to the positive default (5000ms) the way
// OptimizeLimit used to before #715.
func TestRotateTimeZeroDisablesTimeBasedRotation(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1000, CommitLimit: 1, RotateTime: 0})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	dir := ui.(*userIndex).state(inbox).dir
	sealed, current := countShards(t, dir)
	if sealed != 0 || current != 1 {
		t.Fatalf("expected no rotation with RotateTime=0: sealed=%d current=%d", sealed, current)
	}
}

func TestRotationAndOptimize(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 2})
	for uid := uint32(1); uid <= 5; uid++ {
		indexDoc(t, ui, uid, nil, []string{"steady"})
	}
	dir := ui.(*userIndex).state(inbox).dir
	sealed, current := countShards(t, dir)
	if sealed < 2 {
		t.Fatalf("expected rotation to seal shards: sealed=%d current=%d", sealed, current)
	}
	res, err := ui.Lookup(inbox, bodyQuery("steady"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 5 {
		t.Fatalf("lookup across shards = %v", res.Definite)
	}
	// Whole-user optimize is a loop over Mailboxes() under each mailbox's
	// own lock now (#1176); the service does exactly this.
	for _, mbox := range ui.Mailboxes() {
		if err := ui.OptimizeMailbox(mbox); err != nil {
			t.Fatal(err)
		}
	}
	sealed, current = countShards(t, dir)
	if sealed != 1 || current != 0 {
		t.Fatalf("after optimize: sealed=%d current=%d, want 1/0", sealed, current)
	}
	res, err = ui.Lookup(inbox, bodyQuery("steady"))
	if err != nil || len(res.Definite) != 5 {
		t.Fatalf("lookup after optimize = %v (%v)", res.Definite, err)
	}
}

func countShards(t *testing.T, dir string) (sealed, current int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(e.Name(), dbPrefix):
			sealed++
		case strings.HasPrefix(e.Name(), currentPrefix):
			current++
		}
	}
	return sealed, current
}

// TestOptimizeCallbackFiresAtLimit (#715) proves rotate() drives the
// OptimizeNotifier callback: it stays silent below OptimizeLimit and fires
// (with the correct mailbox) as soon as the sealed-shard count reaches it.
func TestOptimizeCallbackFiresAtLimit(t *testing.T) {
	eng := New(Options{RotateCount: 2, OptimizeLimit: 3})
	var mu sync.Mutex
	var calls []fts.MailboxRef
	eng.SetOptimizeCallback(func(_ fts.UserRef, m fts.MailboxRef) {
		mu.Lock()
		calls = append(calls, m)
		mu.Unlock()
	})
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := eng.OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	// RotateCount=2: 4 docs seal exactly 2 shards — below OptimizeLimit=3.
	for uid := uint32(1); uid <= 4; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("callback fired %d times before reaching OptimizeLimit (2 sealed shards)", n)
	}

	// 2 more docs seal a 3rd shard — now at OptimizeLimit.
	for uid := uint32(5); uid <= 6; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("callback never fired after reaching OptimizeLimit")
	}
	if calls[0].GUID != inbox.GUID {
		t.Fatalf("callback mailbox = %+v, want GUID %q", calls[0], inbox.GUID)
	}
}

// TestOptimizeCallbackDisabledWhenLimitZero (#715) proves OptimizeLimit: 0
// truly disables auto-optimize, rather than withDefaults() silently
// coercing it back to the positive default (10) the way it used to.
func TestOptimizeCallbackDisabledWhenLimitZero(t *testing.T) {
	eng := New(Options{RotateCount: 2, OptimizeLimit: 0})
	var calls atomic.Int32
	eng.SetOptimizeCallback(func(fts.UserRef, fts.MailboxRef) { calls.Add(1) })
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := eng.OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	// 30 docs / RotateCount=2 seals 15 shards — well past even the old
	// (buggy) implicit default of 10, so this decisively catches a
	// regression back to "0 silently becomes 10" rather than passing by
	// accident from too few shards.
	for uid := uint32(1); uid <= 30; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("callback fired %d times with OptimizeLimit=0 (must be disabled)", n)
	}
}

// TestOptimizeMailboxIsolatesOtherMailboxes (#715) proves OptimizeMailbox
// compacts exactly the requested mailbox, leaving a different mailbox's
// shards untouched — unlike whole-user Optimize.
func TestOptimizeMailboxIsolatesOtherMailboxes(t *testing.T) {
	ui, user := testEngine(t, Options{RotateCount: 2})
	archive := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}

	for uid := uint32(1); uid <= 4; uid++ {
		indexDocIn(t, ui, inbox, uid, nil, []string{"steady"})
		indexDocIn(t, ui, archive, uid, nil, []string{"steady"})
	}
	dirInbox := ui.(*userIndex).state(inbox).dir
	dirArchive := ui.(*userIndex).state(archive).dir
	sealedInbox, _ := countShards(t, dirInbox)
	sealedArchive, _ := countShards(t, dirArchive)
	if sealedInbox < 2 || sealedArchive < 2 {
		t.Fatalf("expected both mailboxes to have rotated shards: inbox=%d archive=%d", sealedInbox, sealedArchive)
	}

	if err := ui.OptimizeMailbox(inbox); err != nil {
		t.Fatal(err)
	}
	sealedInbox, _ = countShards(t, dirInbox)
	sealedArchive, _ = countShards(t, dirArchive)
	if sealedInbox != 1 {
		t.Fatalf("inbox not optimized: sealed=%d, want 1", sealedInbox)
	}
	if sealedArchive < 2 {
		t.Fatalf("archive must be untouched by OptimizeMailbox(inbox): sealed=%d", sealedArchive)
	}
	_ = user
}

// TestShardPathsIgnoresOptimizeTmpDir (#715) is the direct safety check
// behind the lazy-cleanup design: a leftover "optimize" compaction tmp dir
// must never be mistaken for a shard by shardPaths — otherwise a stale tmp
// dir left by a crash would corrupt every subsequent Lookup/Optimize, not
// just waste disk space until the next lazy cleanup.
func TestShardPathsIgnoresOptimizeTmpDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "optimize"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, dbPrefix+"1"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := shardPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if filepath.Base(p) == "optimize" {
			t.Fatal("shardPaths must not pick up the optimize tmp dir as a shard")
		}
	}
	if len(paths) != 1 {
		t.Fatalf("shardPaths = %v, want exactly the one dbPrefix dir", paths)
	}
}

// TestCleanStaleOptimizeTmpDir (#715) proves a leftover "optimize" tmp dir
// from a prior crash is swept the first time the mailbox's directory is
// touched — the "lazy, on first state() open" substitute for a startup
// sweep the service has no way to do upfront (no list of every mailbox).
func TestCleanStaleOptimizeTmpDir(t *testing.T) {
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := New(Options{}).OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	dir := ui.(*userIndex).eng.opts.MailboxDir(user, inbox)
	tmp := filepath.Join(dir, "optimize")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	indexDoc(t, ui, 1, nil, []string{"hi"}) // first touch of this mailbox's state

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("stale optimize tmp dir not cleaned: stat err=%v", err)
	}
}

func TestSubstringSearch(t *testing.T) {
	ui, _ := testEngine(t, Options{SubstringSearch: true})
	indexDoc(t, ui, 1, nil, []string{"butterfly"})
	// Substring mode stores suffixes, so an inner fragment prefix-matches.
	res, err := ui.Lookup(inbox, bodyQuery("tterf"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Definite, []uint32{1}) {
		t.Fatalf("substring lookup = %v, want [1]", res.Definite)
	}
	// Without substring mode the same fragment must not match.
	ui2, _ := testEngine(t, Options{})
	indexDoc(t, ui2, 1, nil, []string{"butterfly"})
	res, err = ui2.Lookup(inbox, bodyQuery("tterf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 0 {
		t.Fatalf("prefix-only lookup matched inner fragment: %v", res.Definite)
	}
}

func TestMinTermSize(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"a", "ok", "xyz"})
	// 1-byte token is below min_term_size (2) and never indexed.
	res, err := ui.Lookup(inbox, bodyQuery("ok"))
	if err != nil || len(res.Definite) != 1 {
		t.Fatalf("2-byte term should be indexed: %v (%v)", res.Definite, err)
	}
	res, err = ui.Lookup(inbox, bodyQuery("a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 0 {
		t.Fatalf("1-byte term must not be indexed: %v", res.Definite)
	}
}

func TestVersionMetadataWritten(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"word"})
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}
	dir := ui.(*userIndex).state(inbox).dir
	paths, err := shardPaths(dir)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shards: %v", err)
	}
	w, err := xapian.OpenWDB(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got, err := w.GetMetadata(versionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != versionValue {
		t.Fatalf("version metadata = %q, want %q", got, versionValue)
	}
}

// TestMailboxDirDriverAware locks the #654 layout: the fts-flatcurve directory
// is co-located inside the mailbox's driver-aware per-folder index path (the
// same FolderSubpath layout the fileindex uses), not a flat <root>/<folder>.
// The index path is keyed by the folder's GUID, and the mail driver does not
// appear in it: the FTS tree is its own, so a driver migration moves the mail
// and leaves the index where it is (#1183). Every driver, one path.
func TestMailboxDirIsKeyedByGUIDNotDriver(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, inbox.GUID, Label)
	for _, driver := range []string{"mdbox", "sdbox", "maildir", ""} {
		user := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: driver}
		ui, err := New(Options{}).OpenUser(context.Background(), user)
		if err != nil {
			t.Fatalf("driver %q: OpenUser: %v", driver, err)
		}
		if got := ui.(*userIndex).state(inbox).dir; got != want {
			t.Errorf("driver %q: dir = %q, want %q", driver, got, want)
		}
		ui.Close() //nolint:errcheck
	}
}

// TestLegacyDirMigration verifies the rename-on-open migration: an index sitting
// at the pre-#654 flat path is relocated in place to the driver-aware path on
// first access (no reindex, no orphan), and its checkpoint survives the move.
func TestLegacyDirMigration(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, inbox.Name, Label) // <root>/INBOX/fts-flatcurve
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	// v2 checkpoint: "2 <uidvalidity> <last_uid> <checksum>".
	if err := os.WriteFile(filepath.Join(legacy, checkpointFile), []byte("2 9 5 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	user := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: "mdbox"}
	ui, err := New(Options{}).OpenUser(context.Background(), user)
	if err != nil {
		t.Fatalf("OpenUser: %v", err)
	}

	// First access triggers the migration.
	newDir := ui.(*userIndex).state(inbox).dir
	wantNew := filepath.Join(root, inbox.GUID, Label)
	if newDir != wantNew {
		t.Fatalf("new dir = %q, want %q", newDir, wantNew)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy dir still present after migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, checkpointFile)); err != nil {
		t.Errorf("checkpoint not present at new dir: %v", err)
	}
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 5 || uidv != 9 || sum != 3 {
		t.Fatalf("checkpoint after migration = %d/%d/%d/%v, want 5/9/3", last, uidv, sum, err)
	}
}

// TestMultiShardLookupReturnsRealUIDs is the #670 regression: once a mailbox's
// index rotates into more than one shard, a combined-database search would
// report Xapian's interleaved external docids instead of the real UIDs. Force
// rotation (RotateCount 3) so 10 messages span several shards, then assert
// SEARCH returns the actual injected UIDs.
func TestMultiShardLookupReturnsRealUIDs(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 3})
	var want []uint32
	for uid := uint32(1); uid <= 10; uid++ {
		if uid%2 == 0 {
			indexDoc(t, ui, uid, nil, []string{"needle"})
			want = append(want, uid)
		} else {
			indexDoc(t, ui, uid, nil, []string{"filler"})
		}
	}
	dir := ui.(*userIndex).state(inbox).dir
	if sealed, _ := countShards(t, dir); sealed < 2 {
		t.Fatalf("expected rotation to seal ≥2 shards, got sealed=%d", sealed)
	}
	res, err := ui.Lookup(inbox, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Definite, want) {
		t.Fatalf("multi-shard lookup = %v, want %v (real UIDs, not interleaved docids)", res.Definite, want)
	}
}

// TestHeaderExistenceRequiresRealToken (#725 item 6) proves the
// header-existence boolean term is set only once a real (>=MinTermSize)
// token is confirmed for that field, not proactively on SetBuildKey: a
// value that tokenizes to nothing must not satisfy a HEADER existence
// probe.
func TestHeaderExistenceRequiresRealToken(t *testing.T) {
	ui, _ := testEngine(t, Options{MinTermSize: 2})
	up, err := ui.BeginUpdate(inbox)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := up.SetBuildKey(fts.BuildKey{UID: 1, Type: fts.KeyHeader, HdrName: "x-empty"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey: ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("a")); err != nil { // below MinTermSize=2
		t.Fatal(err)
	}
	ok, err = up.SetBuildKey(fts.BuildKey{UID: 1, Type: fts.KeyHeader, HdrName: "x-real"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey: ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}

	probe := func(hdr string) []uint32 {
		t.Helper()
		res, err := ui.Lookup(inbox, fts.Query{
			Terms:    []fts.Term{{Field: fts.FieldHeader, HdrName: hdr}},
			AndTerms: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Definite
	}
	if got := probe("x-empty"); len(got) != 0 {
		t.Fatalf("HEADER x-empty existence probe = %v, want none (zero real tokens)", got)
	}
	if got := probe("x-real"); !reflect.DeepEqual(got, []uint32{1}) {
		t.Fatalf("HEADER x-real existence probe = %v, want [1]", got)
	}
}

// TestHeaderNameIndexedSeparately (#725 item 5) proves the header NAME
// itself is searchable via TEXT (the A-pool), and that it does NOT also
// satisfy a HEADER <name> VALUE search for that same literal name — the
// name and the value are indexed under separate build keys.
func TestHeaderNameIndexedSeparately(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	up, err := ui.BeginUpdate(inbox)
	if err != nil {
		t.Fatal(err)
	}
	// The header name build key: empty HdrName, per buildmail's contract.
	ok, err := up.SetBuildKey(fts.BuildKey{UID: 1, Type: fts.KeyHeader})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey (name): ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("list")); err != nil {
		t.Fatal(err)
	}
	if err := up.BuildMore([]byte("id")); err != nil {
		t.Fatal(err)
	}
	// The value build key: a value that shares no words with the name.
	ok, err = up.SetBuildKey(fts.BuildKey{UID: 1, Type: fts.KeyHeader, HdrName: "list-id"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey (value): ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("project")); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}

	// TEXT "list" matches — the header NAME reached the A-pool.
	res, err := ui.Lookup(inbox, fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldText, Words: []fts.Word{{Variants: []string{"list"}}}}},
		AndTerms: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Definite, []uint32{1}) {
		t.Fatalf("TEXT %q = %v, want [1] (header name must reach the A-pool)", "list", res.Definite)
	}

	// HEADER list-id "list" must NOT match — the name's tokens must not
	// leak into the per-field H<NAME> pool alongside the value.
	res, err = ui.Lookup(inbox, fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldHeader, HdrName: "list-id", Words: []fts.Word{{Variants: []string{"list"}}}}},
		AndTerms: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 0 {
		t.Fatalf("HEADER list-id %q = %v, want none (name tokens must not leak into the value pool)", "list", res.Definite)
	}
}
