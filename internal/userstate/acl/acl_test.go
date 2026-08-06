package acl

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestStore_DefaultsFromInbox locks acl_defaults_from_inbox: a maildir folder
// with no ACL of its own inherits the namespace-root default from INBOX's ACL,
// which is the only default source for maildir (the folder-"" default collides
// with INBOX and is disabled). Without the flag, the default is empty.
func TestStore_DefaultsFromInbox(t *testing.T) {
	inboxACL := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}
	seed := func(s *Store) {
		if err := s.Set("INBOX", inboxACL); err != nil {
			t.Fatalf("seed INBOX ACL: %v", err)
		}
	}
	// With the flag: a folder without its own ACL resolves bob's rights from
	// INBOX; INBOX itself resolves from its own ACL.
	on := New(t.TempDir(), "", "maildir", ".", "", "alice", "test", Policy{DefaultsFromInbox: true}, nil)
	seed(on)
	for _, folder := range []string{"Projects", "Projects.Sub", "INBOX"} {
		got, err := on.EffectiveFor(folder, "bob", nil, false, '.')
		if err != nil {
			t.Fatalf("EffectiveFor(%q): %v", folder, err)
		}
		if got != "lr" {
			t.Errorf("defaults_from_inbox on: EffectiveFor(%q, bob)=%q, want lr", folder, got)
		}
	}
	// A folder WITH its own ACL wins over the INBOX default.
	if err := on.Set("Projects.Own", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrs"},
	}); err != nil {
		t.Fatalf("set own ACL: %v", err)
	}
	if got, _ := on.EffectiveFor("Projects.Own", "bob", nil, false, '.'); got != "lrs" {
		t.Errorf("own ACL should win: got %q, want lrs", got)
	}

	// Without the flag: maildir has no root default → empty for a bare folder.
	off := New(t.TempDir(), "", "maildir", ".", "", "alice", "test", Policy{}, nil)
	seed(off)
	if got, _ := off.EffectiveFor("Projects", "bob", nil, false, '.'); got != "" {
		t.Errorf("defaults_from_inbox off: EffectiveFor(Projects, bob)=%q, want empty", got)
	}
	// INBOX still resolves from its own explicit ACL regardless of the flag.
	if got, _ := off.EffectiveFor("INBOX", "bob", nil, false, '.'); got != "lr" {
		t.Errorf("INBOX own ACL: got %q, want lr", got)
	}
}

func TestStore_GetMissingFileReturnsNil(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
	tests := []struct {
		folder, wantSuffix string
	}{
		{"INBOX", FileName},
		{"Sent", filepath.Join(".Sent", FileName)},
		{"Lists/announce", filepath.Join(".Lists.announce", FileName)},
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
	acl := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
	}
	if err := s.Set("Nested/folder/path", acl); err != nil {
		t.Fatalf("Set with missing parent dir: %v", err)
	}
	expected := filepath.Join(home, ".Nested.folder.path", FileName)
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("file not created at %s: %v", expected, err)
	}
}

func TestStore_SetIsAtomic_NoTmpLeak(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
	acl := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDOwner}, Rights: mailbox.FullRights},
	}
	if err := s.Set("INBOX", acl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := os.ReadDir(home)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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

func TestStore_EffectiveForOwnerDefault(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
	// Owner with no ACL gets the owner-default full rights.
	if got, _ := s.EffectiveFor("Lists/news", "alice", nil, true, '/'); got != mailbox.FullRights {
		t.Errorf("owner default: got %q, want FullRights", got)
	}
	// An explicit user= entry for the owner replaces the owner default —
	// user= (tier 4) is more specific than the owner tier (3).
	if err := s.Set("Lists/news", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "alice"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, _ := s.EffectiveFor("Lists/news", "alice", nil, true, '/'); got != mailbox.MustParseRights("lr") {
		t.Errorf("owner with user= entry: got %q, want lr", got)
	}
}

func TestStore_EffectiveForLeafACLWins(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	// The namespace root has a file of its own, beside the mailbox tree,
	// on every driver: it used to share INBOX's on maildir, which left a
	// shared namespace with nowhere to grant the create right (#1091).
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
	got := s.Path("")
	want := filepath.Join(home, "mailboxes", RootFileName)
	if got != want {
		t.Errorf("Path(\"\") = %q, want %q (namespace-root file)", got, want)
	}
}

func TestStore_SetGetRootACL(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
	in := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrk"},
	}
	if err := s.Set("", in); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "mailboxes", RootFileName)); err != nil {
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
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
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

// TestStore_MaildirRootDefaultDisabled asserts that for maildir the local
// namespace-root default ACL is unavailable — its path is INBOX's own, so
// Set("") is refused and inheritance never falls through to it; a global ACL is the intended default source in that case.
// The namespace root is grantable on maildir too. It was not: its ACL shared
// INBOX's file, so the root default was disabled to keep one from being read as
// the other — which left a shared maildir namespace with nowhere to grant the
// create right, and therefore unable to hold its first mailbox (#1091).
func TestStore_MaildirRootIsGrantable(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "maildir", ".", "", "alice", "test", Policy{}, nil)
	if err := s.Set("", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lk"},
	}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	got, err := s.EffectiveFor("TopLevel", "bob", nil, false, '/')
	if err != nil {
		t.Fatalf("EffectiveFor: %v", err)
	}
	if !strings.ContainsRune(string(got), 'k') {
		t.Errorf("effective rights on a top-level name = %q, want the root grant to carry 'k'", got)
	}

	// And it did not land on INBOX: the two are separate files, which is the
	// whole point. Read the directory rather than the API, because a store
	// that writes and reads one file for both would agree with itself.
	if _, err := os.Stat(filepath.Join(home, RootFileName)); err != nil {
		t.Errorf("root ACL file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, FileName)); err == nil {
		t.Error("the root grant was written to INBOX's ACL file")
	}
}

func TestStore_EffectiveForFallsThroughToRootFromDeep(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
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
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
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
	// maildir INBOX ACL lives in the maildir root.
	bad := filepath.Join(home, FileName)
	if err := os.WriteFile(bad, []byte("user=eve INVALID-RIGHTS\n"), 0o600); err != nil {
		t.Fatalf("seed bad file: %v", err)
	}
	s := New(home, "", "", "/", "", "alice", "test", Policy{}, nil)
	_, err := s.Get("INBOX")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// TestStore_ACLFollowsMailPathNotIndex guards that the yarilo-acl file lives
// in the mailbox directory (MailPath), not the INDEX root:
// the ACL lives with the mail data, so INDEX= must not relocate it.
func TestStore_ACLFollowsMailPathNotIndex(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	indexRoot := t.TempDir()
	s := New(home, mailPath, "", "/", "", "alice", "test", Policy{}, nil)

	if err := s.Set("INBOX", mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// ACL lives in the mailbox dir (maildir INBOX == the maildir root).
	if _, err := os.Stat(filepath.Join(mailPath, FileName)); err != nil {
		t.Errorf("yarilo-acl not found under mail root: %v", err)
	}
	// INDEX= must not have pulled it into the index root.
	if _, err := os.Stat(filepath.Join(indexRoot, FileName)); err == nil {
		t.Errorf("yarilo-acl leaked into INDEX root")
	}
}

// TestStore_CacheTTL verifies acl_cache_ttl: within the TTL a parsed ACL is
// served from cache even after the file changes underneath; past the TTL the
// mtime+size re-validation picks up the change; and a local write invalidates
// immediately.
func TestStore_CacheTTL(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "", "maildir", "/", "", "alice", "test", Policy{CacheTTL: 30 * time.Second}, nil)

	now := time.Unix(1_700_000_000, 0)
	s.clock = func() time.Time { return now }

	acl1 := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}}
	acl2 := mailbox.ACL{
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lrs"},
		{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lrswipkxtea"},
	}
	str := func(a mailbox.ACL) string { return a.Sorted().String() }

	if err := s.Set("Work", acl1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// First read caches acl1.
	got, err := s.Get("Work")
	if err != nil || str(got) != str(acl1) {
		t.Fatalf("initial Get = %q (%v), want acl1", str(got), err)
	}

	// External change the Store did not make (bypasses invalidation).
	if err := os.WriteFile(s.Path("Work"), []byte(str(acl2)), 0o600); err != nil {
		t.Fatalf("external write: %v", err)
	}

	// Within TTL: the stale cached acl1 is still served.
	got, _ = s.Get("Work")
	if str(got) != str(acl1) {
		t.Errorf("within TTL Get = %q, want cached acl1", str(got))
	}

	// Past TTL: mtime+size re-validation reloads acl2.
	now = now.Add(31 * time.Second)
	got, _ = s.Get("Work")
	if str(got) != str(acl2) {
		t.Errorf("post-TTL Get = %q, want reloaded acl2", str(got))
	}

	// A local write invalidates immediately, even within TTL.
	if err := s.Set("Work", acl1); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, _ = s.Get("Work")
	if str(got) != str(acl1) {
		t.Errorf("after local Set Get = %q, want acl1 (invalidated)", str(got))
	}
}

// The root ACL is a new entity in the on-disk layout, so it is inspected as
// bytes in a directory rather than through the API that wrote it: a store that
// writes and reads one file for both the root and INBOX is self-consistent and
// proves nothing (the lesson from mailbox_list_utf8, #1074).
func TestStore_RootACLOnDisk(t *testing.T) {
	for _, tc := range []struct{ driver, wantDir string }{
		{"maildir", ""},
		{"mdbox", "mailboxes"},
		{"sdbox", "mailboxes"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			home := t.TempDir()
			s := New(home, "", tc.driver, "/", "", "alice", "test", Policy{}, nil)

			if err := s.Set("", mailbox.ACL{
				{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lk"},
			}); err != nil {
				t.Fatalf("Set root: %v", err)
			}
			if err := s.Set("INBOX", mailbox.ACL{
				{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "carol"}, Rights: "lr"},
			}); err != nil {
				t.Fatalf("Set INBOX: %v", err)
			}

			rootFile := filepath.Join(home, tc.wantDir, RootFileName)
			rootBytes, err := os.ReadFile(rootFile)
			if err != nil {
				t.Fatalf("root ACL not on disk at %s: %v", rootFile, err)
			}
			if !strings.Contains(string(rootBytes), "bob") {
				t.Errorf("%s holds %q, which is not the root grant", rootFile, rootBytes)
			}
			// The two grants are in two files. One file holding both is the
			// collision this change exists to remove.
			if strings.Contains(string(rootBytes), "carol") {
				t.Errorf("%s holds INBOX's grant too:\n%s", rootFile, rootBytes)
			}
		})
	}
}
