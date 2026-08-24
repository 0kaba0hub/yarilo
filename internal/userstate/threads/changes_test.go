package threads

import (
	"fmt"
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

// Compaction rewrites the log, so a position from before it names records of a
// history that no longer exists -- and the number stays valid, which is what
// makes this dangerous rather than merely wrong. Before the generation
// existed, this input produced a confident diff of unrelated records.
func TestAPositionFromBeforeACompactionIsDetectable(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Enough records that the compacted file is not shorter than the client's
	// position: a shorter file is caught by the head check alone, and that
	// case would hide the one under test.
	for i := 0; i < 6; i++ {
		mustAppend(t, path, st, Placement{
			GUID:       fmt.Sprintf("g%d", i),
			MessageID:  fmt.Sprintf("m%d@x", i),
			SubjectKey: fmt.Sprintf("subject %d", i),
			ThreadID:   fmt.Sprintf("g%d", i),
		})
	}
	clientAt := 4
	genBefore := st.Generation()

	if err := Compact(path, st); err != nil {
		t.Fatal(err)
	}
	ch, err := ChangesSince(path, clientAt)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Head < clientAt {
		t.Fatalf("the compacted file is shorter than the client's position (%d < %d): "+
			"this run cannot show the generation doing anything", ch.Head, clientAt)
	}
	if ch.Generation == genBefore {
		t.Errorf("generation = %d, unchanged by a compaction: a client's stale position would read as valid", ch.Generation)
	}
}

// And a position from the current generation still answers, so the check does
// not simply refuse everything after the first compaction.
func TestAPositionFromTheCurrentGenerationStillAnswers(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "g1"})
	if err := Compact(path, st); err != nil {
		t.Fatal(err)
	}
	at := st.Head()
	gen := st.Generation()
	mustAppend(t, path, st, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "g2"})

	ch, err := ChangesSince(path, at)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Generation != gen {
		t.Errorf("generation = %d, want %d: an append must not look like a compaction", ch.Generation, gen)
	}
	if len(ch.Created) != 1 || ch.Created[0] != "g2" {
		t.Errorf("created = %v, want [g2]", ch.Created)
	}
}
