package subs

import (
	"sort"
	"testing"
)

func TestSubsAddSnapshotRemove(t *testing.T) {
	home := t.TempDir()
	store := New(home, "subscriptions", "alice@example.com", "test/1/alice@example.com", nil)

	cases := []struct {
		op       string
		folder   string
		expected []string
	}{
		{"add", "INBOX", []string{"INBOX"}},
		{"add", "Sent", []string{"INBOX", "Sent"}},
		{"add", "Sent", []string{"INBOX", "Sent"}}, // idempotent
		{"remove", "Sent", []string{"INBOX"}},
		{"remove", "Sent", []string{"INBOX"}}, // idempotent
	}
	for _, c := range cases {
		var err error
		switch c.op {
		case "add":
			err = store.Add(c.folder)
		case "remove":
			err = store.Remove(c.folder)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", c.op, c.folder, err)
		}
		snap, err := store.Snapshot()
		if err != nil {
			t.Fatalf("snapshot after %s %s: %v", c.op, c.folder, err)
		}
		got := make([]string, 0, len(snap))
		for k := range snap {
			got = append(got, k)
		}
		sort.Strings(got)
		if !equalSlices(got, c.expected) {
			t.Errorf("after %s %s: got %v want %v", c.op, c.folder, got, c.expected)
		}
	}
}

func TestSubsSnapshotEmptyWhenMissing(t *testing.T) {
	home := t.TempDir()
	store := New(home, "subscriptions", "alice", "owner", nil)
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 0 {
		t.Errorf("snapshot=%v want empty", snap)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
