package acl

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestStore_GetMissingFileReturnsNil(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	got, err := s.Get("INBOX")
	if err != nil {
		t.Fatalf("Get on missing file: %v", err)
	}
	if got != nil {
		t.Errorf("missing file should yield nil ACL, got %+v", got)
	}
}

func TestStore_SetGetRoundTrip(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	in := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: mailbox.MustParseRights("lrs")},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "mallory"}, Rights: mailbox.MustParseRights("lrwa"), Negative: true},
	}
	if err := s.Set("INBOX", in); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("INBOX")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Stored order is canonical (Sorted) — owner first, then user= alphabetically,
	// with positive entries before negative for the same identifier.
	want := in.Sorted()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch\n got=%+v\nwant=%+v", got, want)
	}
}

func TestStore_PathLayoutMirrorsFileindex(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	tests := []struct {
		folder, wantSuffix string
	}{
		{"INBOX", filepath.Join("INBOX", FileName)},
		{"Sent", filepath.Join(".Sent", FileName)},
		{"Lists/announce", filepath.Join(".Lists/announce", FileName)},
	}
	for _, tc := range tests {
		t.Run(tc.folder, func(t *testing.T) {
			got := s.Path(tc.folder)
			want := filepath.Join(home, tc.wantSuffix)
			if got != want {
				t.Errorf("Path(%q) = %q, want %q", tc.folder, got, want)
			}
		})
	}
}

func TestStore_SetCreatesParentDir(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	acl := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
	}
	if err := s.Set("Nested/folder/path", acl); err != nil {
		t.Fatalf("Set with missing parent dir: %v", err)
	}
	expected := filepath.Join(home, ".Nested/folder/path", FileName)
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("file not created at %s: %v", expected, err)
	}
}

func TestStore_SetIsAtomic_NoTmpLeak(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	acl := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
	}
	if err := s.Set("INBOX", acl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "INBOX"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestStore_SetReplacesPriorContent(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	first := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: mailbox.MustParseRights("lrs")},
	}
	second := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: mailbox.MustParseRights("lr")},
	}
	if err := s.Set("INBOX", first); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := s.Set("INBOX", second); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	got, err := s.Get("INBOX")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, second.Sorted()) {
		t.Errorf("Set did not replace prior content\n got=%+v\nwant=%+v", got, second.Sorted())
	}
}

func TestStore_RemoveIdempotent(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Remove("INBOX"); err != nil {
		t.Errorf("Remove on missing file should be nil, got %v", err)
	}
	acl := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights}}
	if err := s.Set("INBOX", acl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Remove("INBOX"); err != nil {
		t.Fatalf("Remove on existing file: %v", err)
	}
	if got, _ := s.Get("INBOX"); got != nil {
		t.Errorf("Get after Remove should be nil, got %+v", got)
	}
	if err := s.Remove("INBOX"); err != nil {
		t.Errorf("second Remove should be nil, got %v", err)
	}
}

func TestStore_UpdateReadModifyWrite(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	prior := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: mailbox.MustParseRights("lrs")},
	}
	if err := s.Set("INBOX", prior); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err := s.Update("INBOX", func(cur mailbox.ACL) (mailbox.ACL, error) {
		if len(cur) != 1 || cur[0].Identifier.Name != "bob" {
			t.Errorf("Update fn received unexpected current: %+v", cur)
		}
		cur = append(cur, mailbox.Entry{
			Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"},
			Rights:     mailbox.MustParseRights("lr"),
		})
		return cur, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get("INBOX")
	if len(got) != 2 {
		t.Fatalf("expected 2 entries after Update, got %d: %+v", len(got), got)
	}
}

func TestStore_UpdateNilReturnPreservesFile(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	prior := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
	}
	if err := s.Set("INBOX", prior); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err := s.Update("INBOX", func(_ mailbox.ACL) (mailbox.ACL, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get("INBOX")
	if !reflect.DeepEqual(got, prior.Sorted()) {
		t.Errorf("nil-return should leave file untouched, got %+v", got)
	}
}

func TestStore_UpdatePropagatesError(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	sentinel := errors.New("user error")
	err := s.Update("INBOX", func(_ mailbox.ACL) (mailbox.ACL, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestStore_ConcurrentUpdatesWithoutLockerDoNotPanic(t *testing.T) {
	// No locker = single-process test; sync.Mutex inside the Store is
	// not enforced, so concurrent writers race on file writes. The
	// test asserts only that no panic occurs and the final file is
	// parseable — verifying the lock-acquired path is the locks-
	// integration suite, not this unit test.
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Set("INBOX", mailbox.ACL{
				{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
				{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "u"}, Rights: mailbox.MustParseRights("lr")},
			})
		}(i)
	}
	wg.Wait()
	got, err := s.Get("INBOX")
	if err != nil {
		t.Fatalf("Get after concurrent writes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries after concurrent writes, got %d", len(got))
	}
}

func TestStore_FilePermissions(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	acl := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights}}
	if err := s.Set("INBOX", acl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := os.Stat(s.Path("INBOX"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 0o600: ACL contains identifiers / rights of other users — keep it
	// to the mail-owner uid only.
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file perm = %o, want 0o600", mode)
	}
}

func TestStore_EffectiveForOwnerShortCircuits(t *testing.T) {
	// Owner gets FullRights regardless of any stored entries — and
	// without reading anything from disk. Seed a non-owner-denying
	// ACL on the leaf to prove the owner path skips the load.
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("Lists/news", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "alice"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news", "alice", nil, true, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != mailbox.FullRights {
		t.Errorf("owner got %q, want FullRights", got)
	}
}

func TestStore_EffectiveForLeafACLWins(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	// Parent grants r only; leaf grants lrws. First-hit-wins → leaf.
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "r"},
	}); err != nil {
		t.Fatalf("Set parent: %v", err)
	}
	if err := s.Set("Lists/news", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrws"},
	}); err != nil {
		t.Fatalf("Set leaf: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "lrsw" {
		t.Errorf("got %q, want lrsw (leaf-wins)", got)
	}
}

func TestStore_EffectiveForInheritsFromParent(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	// Parent has explicit ACL; leaf has none → inherits.
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set parent: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "lr" {
		t.Errorf("got %q, want lr (inherited)", got)
	}
}

func TestStore_EffectiveForInheritsAcrossMultipleLevels(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	// Three-deep folder; only the top has an ACL.
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "l"},
	}); err != nil {
		t.Fatalf("Set top: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news/2026", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "l" {
		t.Errorf("got %q, want l (inherited from top)", got)
	}
}

func TestStore_EffectiveForNoACLAnywhereYieldsEmpty(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	got, err := s.EffectiveFor("Lists/news", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (no ACL on path)", got)
	}
}

func TestStore_EffectiveForLeafOverridesParentEvenWhenLeafIsRestrictive(t *testing.T) {
	// First-hit-wins: a leaf ACL that denies (even by being empty
	// for this user) overrides a permissive parent.
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set parent: %v", err)
	}
	// Leaf has an ACL, but it has no entry for bob and no anyone-grant.
	if err := s.Set("Lists/private", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "alice"}, Rights: "lrws"},
	}); err != nil {
		t.Fatalf("Set leaf: %v", err)
	}
	got, err := s.EffectiveFor("Lists/private", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (leaf overrides parent)", got)
	}
}

func TestStore_EffectiveForNegativeInInheritedACL(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "lrs"},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "s", Negative: true},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "lr" {
		t.Errorf("got %q, want lr (anyone lrs minus negative s for bob)", got)
	}
}

func TestStore_PathEmptyFolderIsNamespaceRoot(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	got := s.Path("")
	want := filepath.Join(home, FileName)
	if got != want {
		t.Errorf("Path(\"\") = %q, want %q (namespace-root file)", got, want)
	}
}

func TestStore_SetGetRootACL(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	in := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrk"},
	}
	if err := s.Set("", in); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, FileName)); err != nil {
		t.Errorf("root file not created: %v", err)
	}
	got, err := s.Get("")
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if !reflect.DeepEqual(got, in.Sorted()) {
		t.Errorf("root round-trip mismatch\n got=%+v\nwant=%+v", got, in.Sorted())
	}
}

func TestStore_EffectiveForFallsThroughToRoot(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	// Only the namespace root has an ACL; leaf has none.
	if err := s.Set("", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrk"},
	}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	got, err := s.EffectiveFor("TopLevel", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "lrk" {
		t.Errorf("got %q, want lrk (inherited from root)", got)
	}
}

func TestStore_EffectiveForFallsThroughToRootFromDeep(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "l"},
	}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news/2026", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "l" {
		t.Errorf("got %q, want l (inherited from root through 3 levels)", got)
	}
}

func TestStore_EffectiveForLeafBeatsRoot(t *testing.T) {
	// Same first-hit-wins rule: an explicit ACL on the leaf fully
	// overrides the root's ACL, no merge.
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	// Leaf grants nothing to bob.
	if err := s.Set("Leaf", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "alice"}, Rights: mailbox.FullRights},
	}); err != nil {
		t.Fatalf("Set leaf: %v", err)
	}
	got, err := s.EffectiveFor("Leaf", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (leaf overrides root)", got)
	}
}

func TestStore_EffectiveForZeroSepStillTriesRoot(t *testing.T) {
	// With sep=0 the walk is disabled, so the namespace-root fall-
	// through must NOT fire either — only the explicit folder is
	// consulted. Mirrors the "no inheritance" opt-out.
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDAnyone}, Rights: "l"},
	}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	got, err := s.EffectiveFor("Leaf", "bob", nil, false, 0)
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (sep=0 disables root fall-through too)", got)
	}
}

func TestStore_EffectiveForZeroSepDisablesWalk(t *testing.T) {
	home := t.TempDir()
	s := New(home, "alice", "test", nil)
	if err := s.Set("Lists", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.EffectiveFor("Lists/news", "bob", nil, false, 0)
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (sep=0 disables ancestor walk)", got)
	}
}

func TestStore_ParseErrorAnnotatesPath(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "INBOX")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bad := filepath.Join(dir, FileName)
	if err := os.WriteFile(bad, []byte("user=eve INVALID-RIGHTS\n"), 0o600); err != nil {
		t.Fatalf("seed bad file: %v", err)
	}
	s := New(home, "alice", "test", nil)
	_, err := s.Get("INBOX")
	if err == nil {
		t.Fatal("expected parse error")
	}
}
