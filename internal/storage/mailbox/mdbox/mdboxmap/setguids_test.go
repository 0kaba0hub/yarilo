package mdboxmap_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// SetGUIDs fills in what a converted store lacks and does not touch what it
// already has.
//
// Both halves matter and only one is obvious. The GUID is the message's
// identity: a record that carries one is answering a question already settled,
// and overwriting it renames a message that other state -- EMAILID a client
// cached, a rebuild's pairing -- refers to by that name (#1573).
func TestSetGUIDsFillsTheEmptyOnesAndLeavesTheRestAlone(t *testing.T) {
	dir := t.TempDir()
	m, err := mdboxmap.Open(dir, "u1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close() //nolint:errcheck

	existing := [16]byte{1, 2, 3, 4}
	withGUID, err := m.AppendRecord(1, 16, 100, existing)
	if err != nil {
		t.Fatal(err)
	}
	var none [16]byte
	without, err := m.AppendRecord(1, 116, 100, none)
	if err != nil {
		t.Fatal(err)
	}

	fresh := [16]byte{9, 9, 9, 9}
	other := [16]byte{7, 7, 7, 7}
	n, err := m.SetGUIDs(map[uint32][16]byte{without: fresh, withGUID: other})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if n != 1 {
		t.Errorf("wrote %d guids, and exactly one record had none", n)
	}

	got := map[uint32][16]byte{}
	for _, r := range m.Records() {
		got[r.UID] = r.GUID
	}
	if got[without] != fresh {
		t.Errorf("the record that had no guid carries %x, want %x", got[without], fresh)
	}
	if got[withGUID] != existing {
		t.Errorf("the record that had one carries %x, and it was %x before", got[withGUID], existing)
	}
}

// A guid that survives a reopen: the stamp is persisted, not only in memory.
func TestSetGUIDsPersists(t *testing.T) {
	dir := t.TempDir()
	m, err := mdboxmap.Open(dir, "u1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var none [16]byte
	uid, err := m.AppendRecord(1, 16, 100, none)
	if err != nil {
		t.Fatal(err)
	}
	want := [16]byte{5, 5, 5, 5}
	if _, err := m.SetGUIDs(map[uint32][16]byte{uid: want}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	again, err := mdboxmap.Open(dir, "u1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close() //nolint:errcheck
	e, ok, err := again.Lookup(uid)
	if err != nil || !ok {
		t.Fatalf("lookup after reopen: ok=%v err=%v", ok, err)
	}
	if e.GUID != want {
		t.Errorf("after a reopen the record carries %x, want %x", e.GUID, want)
	}
}
