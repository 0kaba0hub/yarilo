//go:build flatcurve

package flatcurve

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/fts"
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

// indexDoc feeds one message's tokens: subject header + body words.
func indexDoc(t *testing.T, ui fts.UserIndex, uid uint32, subject []string, body []string) {
	t.Helper()
	up, err := ui.BeginUpdate(inbox)
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
	ui, user := testEngine(t, Options{})
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
	_ = user
}

func TestCheckpointMigrationFallback(t *testing.T) {
	// A migrated index has no yarilo checkpoint file: last UID must
	// come from Xapian's lastdocid.
	ui, user := testEngine(t, Options{})
	indexDoc(t, ui, 17, nil, []string{"legacy"})
	dir := filepath.Join(user.IndexRoot, inbox.Name, Label)
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

func TestRotationAndOptimize(t *testing.T) {
	ui, user := testEngine(t, Options{RotateCount: 2})
	for uid := uint32(1); uid <= 5; uid++ {
		indexDoc(t, ui, uid, nil, []string{"steady"})
	}
	dir := filepath.Join(user.IndexRoot, inbox.Name, Label)
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
	if err := ui.Optimize(); err != nil {
		t.Fatal(err)
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
	ui, user := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"word"})
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(user.IndexRoot, inbox.Name, Label)
	paths, err := shardPaths(dir)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shards: %v", err)
	}
	w, err := openWDB(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	got, err := w.getMetadata(versionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != versionValue {
		t.Fatalf("version metadata = %q, want %q", got, versionValue)
	}
}
