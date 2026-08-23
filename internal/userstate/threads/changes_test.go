package threads

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The log is the change journal for conversations, so what a client learns is
// the records between its position and the head. These rows pin what each
// record means to a client, which is not the same as what it means to us.
func TestChangesSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"})
	mustAppend(t, path, st, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "g2"})
	mark := st.Head()

	t.Run("a message added to a known conversation updates it", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), FileName)
		s, _ := Load(p)
		mustAppend(t, p, s, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"})
		at := s.Head()
		mustAppend(t, p, s, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "g1"})

		ch, cerr := ChangesSince(p, at)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if len(ch.Created) != 0 {
			t.Errorf("created = %v, want none: the conversation already existed", ch.Created)
		}
		if !reflect.DeepEqual(ch.Updated, []string{"g1"}) {
			t.Errorf("updated = %v, want [g1]", ch.Updated)
		}
	})

	t.Run("a merge destroys the swallowed and updates the survivor", func(t *testing.T) {
		ch, cerr := ChangesSince(path, mark)
		if cerr != nil {
			t.Fatal(cerr)
		}
		mustAppend(t, path, st, Placement{GUID: "g3", MessageID: "c@x", ThreadID: "g1", MergedFrom: []string{"g2"}})
		ch, cerr = ChangesSince(path, mark)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if !reflect.DeepEqual(ch.Destroyed, []string{"g2"}) {
			t.Errorf("destroyed = %v, want [g2]", ch.Destroyed)
		}
		if !reflect.DeepEqual(ch.Updated, []string{"g1"}) {
			t.Errorf("updated = %v, want [g1]", ch.Updated)
		}
	})

	// The row a plain merge test cannot reach: a conversation that appeared
	// AND was swallowed since the client's state was never seen by that
	// client, so telling it to destroy the id is telling it about something it
	// does not have. The mutation that reports it anyway passes every other
	// row here.
	t.Run("a conversation created and swallowed in the same window is not destroyed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), FileName)
		s, _ := Load(p)
		mustAppend(t, p, s, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"})
		at := s.Head()

		// Both of these happen after the client's state.
		mustAppend(t, p, s, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "g2"})
		mustAppend(t, p, s, Placement{GUID: "g3", MessageID: "c@x", ThreadID: "g1", MergedFrom: []string{"g2"}})

		ch, cerr := ChangesSince(p, at)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if len(ch.Destroyed) != 0 {
			t.Errorf("destroyed = %v, want none: the client never saw that conversation", ch.Destroyed)
		}
		for _, id := range ch.Created {
			if id == "g2" {
				t.Errorf("created = %v, still names a conversation that no longer exists", ch.Created)
			}
		}
	})

	t.Run("nothing since the head", func(t *testing.T) {
		ch, cerr := ChangesSince(path, st.Head())
		if cerr != nil {
			t.Fatal(cerr)
		}
		if len(ch.Created)+len(ch.Updated)+len(ch.Destroyed) != 0 {
			t.Errorf("changes = %+v, want none", ch)
		}
		if ch.Head != st.Head() {
			t.Errorf("head = %d, want %d", ch.Head, st.Head())
		}
	})

	// An account with no sidecar has no history, and saying so beats an error:
	// it is the state of every account the migration has not reached.
	t.Run("no sidecar", func(t *testing.T) {
		ch, cerr := ChangesSince(filepath.Join(t.TempDir(), "absent"), 0)
		if cerr != nil {
			t.Fatalf("a missing sidecar is an error: %v", cerr)
		}
		if ch.Head != 0 {
			t.Errorf("head = %d, want 0", ch.Head)
		}
	})
}
