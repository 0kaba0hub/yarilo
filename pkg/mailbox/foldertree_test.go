package mailbox

import (
	"os"
	"path/filepath"
	"testing"
)

// makeDirs creates each path under root.
func makeDirs(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(p)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWalkDboxTree exercises the sdbox-style layout: a directory is selectable
// when it owns a dbox-Mails leaf; the leaf is not a hierarchy child; \NoSelect
// containers are derived from selectable descendants; empty strays are ignored.
func TestWalkDboxTree(t *testing.T) {
	root := t.TempDir()
	makeDirs(t, root,
		"INBOX/dbox-Mails",     // selectable top-level
		"Work/dbox-Mails",      // selectable parent
		"Work/Proj/dbox-Mails", // selectable nested child
		"Cont/Deep/dbox-Mails", // Cont is a \NoSelect container of Deep
		"Stray",                // empty, no dbox-Mails, no children → ignored
		"Stray2/AlsoStray",     // no dbox-Mails anywhere below → ignored
	)
	decode := func(s string) (string, bool) { return s, true }
	isMarker := func(name string) bool { return name == "dbox-Mails" }
	selectable := func(diskRel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(diskRel), "dbox-Mails"))
		return err == nil
	}
	entries, err := WalkDboxTree(root, ".", decode, isMarker, selectable)
	if err != nil {
		t.Fatalf("WalkDboxTree: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.Selectable
	}
	want := map[string]bool{
		"INBOX":     true,
		"Work":      true,
		"Work.Proj": true,
		"Cont":      false, // \NoSelect container
		"Cont.Deep": true,
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want keys %v", got, want)
	}
	for name, sel := range want {
		v, ok := got[name]
		if !ok {
			t.Errorf("missing folder %q", name)
			continue
		}
		if v != sel {
			t.Errorf("folder %q selectable=%v, want %v", name, v, sel)
		}
	}
	for _, stray := range []string{"Stray", "Stray2", "Stray2.AlsoStray"} {
		if _, ok := got[stray]; ok {
			t.Errorf("stray dir %q must not be listed", stray)
		}
	}
}

// TestWalkDboxTreeParentBeforeChild locks that a derived container precedes its
// child so LIST emits the hierarchy top-down.
func TestWalkDboxTreeParentBeforeChild(t *testing.T) {
	root := t.TempDir()
	makeDirs(t, root, "A/B/dbox-Mails")
	decode := func(s string) (string, bool) { return s, true }
	isMarker := func(name string) bool { return name == "dbox-Mails" }
	selectable := func(diskRel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(diskRel), "dbox-Mails"))
		return err == nil
	}
	entries, err := WalkDboxTree(root, ".", decode, isMarker, selectable)
	if err != nil {
		t.Fatalf("WalkDboxTree: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "A" || entries[0].Selectable ||
		entries[1].Name != "A.B" || !entries[1].Selectable {
		t.Fatalf("want [A(noselect) A.B(select)], got %v", entries)
	}
}
