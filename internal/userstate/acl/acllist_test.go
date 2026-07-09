package acl

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestList_SnapshotEmptyReturnsNil(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	got, err := s.ListSnapshot()
	if err != nil {
		t.Fatalf("ListSnapshot: %v", err)
	}
	if got != nil {
		t.Errorf("missing file should yield nil snapshot, got %+v", got)
	}
}

func TestList_SetPopulatesIndex(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	acl := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: mailbox.MustParseRights("lr")},
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: mailbox.MustParseRights("l")},
	}
	if err := s.Set("Shared/team", acl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := s.ListSnapshot()
	if err != nil {
		t.Fatalf("ListSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Mailbox != "Shared/team" {
			t.Errorf("unexpected mailbox %q", e.Mailbox)
		}
	}
}

func TestList_RoundTripAcrossInstance(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	if err := s.Set("A", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := s.Set("B", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lrs"},
	}); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	// Re-open Store to confirm the file is the only source of truth.
	s2 := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	entries, err := s2.ListSnapshot()
	if err != nil {
		t.Fatalf("ListSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(entries), entries)
	}
	gotByBox := map[string]string{}
	for _, e := range entries {
		gotByBox[e.Mailbox] = e.Identifier.Name + "=" + e.Rights.String()
	}
	want := map[string]string{
		"A": "bob=lr",
		"B": "carol=lrs",
	}
	if !reflect.DeepEqual(gotByBox, want) {
		t.Errorf("got %v, want %v", gotByBox, want)
	}
}

func TestList_SetReplaceDropsOldEntries(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	if err := s.Set("Shared/team", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := s.Set("Shared/team", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "dan"}, Rights: "lrs"},
	}); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	entries, _ := s.ListSnapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after replace, got %d: %+v", len(entries), entries)
	}
	if entries[0].Identifier.Name != "dan" {
		t.Errorf("expected dan, got %+v", entries[0])
	}
}

func TestList_RemoveDropsAllEntriesForMailbox(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	if err := s.Set("A", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := s.Set("B", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "dan"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	if err := s.Remove("A"); err != nil {
		t.Fatalf("Remove A: %v", err)
	}
	entries, _ := s.ListSnapshot()
	if len(entries) != 1 || entries[0].Mailbox != "B" {
		t.Errorf("expected only B to remain, got %+v", entries)
	}
}

func TestList_RenameRewritesMailboxField(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	if err := s.Set("Old", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Pre-create destination dir so Rename's MkdirAll has somewhere safe to move to.
	if err := os.MkdirAll(filepath.Join(home, ".New"), 0o700); err != nil {
		t.Fatalf("mkdir new dir: %v", err)
	}
	if err := s.Rename("Old", "New"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	entries, _ := s.ListSnapshot()
	if len(entries) != 1 || entries[0].Mailbox != "New" {
		t.Errorf("expected entry to point at New, got %+v", entries)
	}
	// Per-mailbox file followed the rename.
	if _, err := os.Stat(s.Path("New")); err != nil {
		t.Errorf("per-mailbox file missing at new path: %v", err)
	}
	if _, err := os.Stat(s.Path("Old")); !os.IsNotExist(err) {
		t.Errorf("per-mailbox file still present at old path: %v", err)
	}
}

func TestList_RenameMissingFileIsNoop(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	if err := s.Rename("Old", "New"); err != nil {
		t.Errorf("Rename of missing file should be nil, got %v", err)
	}
}

func TestList_LookupAppliesEffectiveSemantics(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	// Box A: bob has 'lr' — visible to lookup.
	// Box B: bob has only 's' (no 'l') — NOT visible.
	// Box C: anyone has 'l', bob has '-l' negative — masked.
	// Box D: bob has nothing — NOT visible.
	if err := s.Set("A", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := s.Set("B", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "s"},
	}); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	if err := s.Set("C", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "l"},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "l", Negative: true},
	}); err != nil {
		t.Fatalf("Set C: %v", err)
	}
	if err := s.Set("D", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set D: %v", err)
	}
	visible, err := s.ListLookup("bob", nil)
	if err != nil {
		t.Fatalf("ListLookup: %v", err)
	}
	gotNames := make([]string, 0, len(visible))
	for k := range visible {
		gotNames = append(gotNames, k)
	}
	sort.Strings(gotNames)
	wantNames := []string{"A"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("got %v, want %v", gotNames, wantNames)
	}
}

func TestList_SortedDeterministic(t *testing.T) {
	// Two stores in the same home should produce byte-identical files
	// when given the same entries in any order.
	home1 := t.TempDir()
	home2 := t.TempDir()
	s1 := New(home1, "", "", "/", "alice", "test", Policy{}, nil)
	s2 := New(home2, "", "", "/", "alice", "test", Policy{}, nil)
	for _, mbox := range []string{"Z", "A", "M"} {
		_ = s1.Set(mbox, mailbox.ACL{
			{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
		})
	}
	// Second store seeds in reverse order.
	for _, mbox := range []string{"M", "A", "Z"} {
		_ = s2.Set(mbox, mailbox.ACL{
			{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
		})
	}
	body1, _ := os.ReadFile(s1.ListPath())
	body2, _ := os.ReadFile(s2.ListPath())
	if string(body1) != string(body2) {
		t.Errorf("non-deterministic order:\n s1=%q\n s2=%q", body1, body2)
	}
}

func TestList_ParseErrorAnnotatesLine(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ListFileName),
		[]byte("A\tuser=bob\tlr\nB\tuser=carol\tINVALID\n"), 0o600); err != nil {
		t.Fatalf("seed bad file: %v", err)
	}
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	_, err := s.ListSnapshot()
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error %q should mention line 2", err)
	}
}

func TestList_RebuildSeedsFromCallback(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "alice", "test", Policy{}, nil)
	// Seed inconsistent state directly via the file (skipping Set),
	// then rebuild from a callback that knows the truth.
	if err := os.WriteFile(filepath.Join(home, ListFileName),
		[]byte("Stale\tuser=ghost\tlr\n"), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	truth := map[string]mailbox.ACL{
		"A": {{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}},
		"B": {{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lrs"}},
	}
	folders := []string{"A", "B"}
	err := s.ListRebuild(folders, func(folder string) (mailbox.ACL, error) {
		return truth[folder], nil
	})
	if err != nil {
		t.Fatalf("ListRebuild: %v", err)
	}
	entries, _ := s.ListSnapshot()
	if len(entries) != 2 {
		t.Errorf("expected 2 entries after rebuild, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Mailbox == "Stale" {
			t.Errorf("stale entry survived rebuild: %+v", e)
		}
	}
}
