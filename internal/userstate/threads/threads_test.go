package threads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), FileName)
}

// An account whose sidecar has not been written yet is every account until the
// migration step reaches it (#1425, backfill only -- no lazy path). It must
// read as "nothing threaded", not as an error, or every JMAP read on an
// unmigrated account fails instead of degrading to one-message-one-thread.
func TestAMissingSidecarIsAnEmptyState(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a missing sidecar is an error: %v", err)
	}
	if _, ok := st.ThreadOfGUID("anything"); ok {
		t.Error("an empty state claims to know a thread")
	}
	if got := st.Threads(); len(got) != 0 {
		t.Errorf("threads = %v, want none", got)
	}
}

func TestAPlacementSurvivesAReload(t *testing.T) {
	path := storePath(t)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	err = Append(path, st, Placement{
		GUID: "g1", MessageID: "root@a", SubjectKey: "project status", ThreadID: "T1",
	})
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := reloaded.ThreadOfGUID("g1"); !ok || id != "T1" {
		t.Errorf("ThreadOfGUID = %q/%v, want T1/true", id, ok)
	}
	if id, ok := reloaded.ThreadOfMessage("root@a"); !ok || id != "T1" {
		t.Errorf("ThreadOfMessage = %q/%v, want T1/true", id, ok)
	}
	if id, ok := reloaded.ThreadOfSubject("project status"); !ok || id != "T1" {
		t.Errorf("ThreadOfSubject = %q/%v, want T1/true", id, ok)
	}
}

// A merge writes an alias instead of rewriting every message of the old
// thread -- that is what keeps it O(1) on the delivery path. The id a client
// cached must therefore still answer, and answer with the conversation that
// survived.
func TestAMergedThreadIdStillAnswers(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)

	mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "T1"})
	mustAppend(t, path, st, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "T2"})
	// The late message that joins them.
	mustAppend(t, path, st, Placement{GUID: "g3", MessageID: "late@x", ThreadID: "T1", MergedFrom: []string{"T2"}})

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// The message that was in T2 now reports T1.
	if id, _ := reloaded.ThreadOfGUID("g2"); id != "T1" {
		t.Errorf("the merged message reports %q, want T1", id)
	}
	// And a client asking about the old id gets the conversation, not nothing.
	if got := reloaded.GUIDsOfThread("T2"); len(got) != 3 {
		t.Errorf("GUIDsOfThread(T2) = %v, want all three messages", got)
	}
	if got := reloaded.Threads(); len(got) != 1 || got[0] != "T1" {
		t.Errorf("threads = %v, want exactly [T1]", got)
	}
}

// Merges chain: a conversation that swallowed another can itself be swallowed.
// Resolution has to follow the whole chain, or the oldest id a client holds
// stops answering exactly when the account is busiest.
func TestAliasesChain(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{GUID: "g1", ThreadID: "T1"})
	mustAppend(t, path, st, Placement{GUID: "g2", ThreadID: "T2", MergedFrom: []string{"T1"}})
	mustAppend(t, path, st, Placement{GUID: "g3", ThreadID: "T3", MergedFrom: []string{"T2"}})

	reloaded, _ := Load(path)
	if id, _ := reloaded.ThreadOfGUID("g1"); id != "T3" {
		t.Errorf("through two merges the message reports %q, want T3", id)
	}
}

// Compaction folds the aliases away. The state it produces must answer exactly
// as the state it replaced -- a compaction that changes an answer is data loss
// with a tidier file.
func TestCompactionKeepsEveryAnswer(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", SubjectKey: "hello", ThreadID: "T1"})
	mustAppend(t, path, st, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "T2"})
	mustAppend(t, path, st, Placement{GUID: "g3", MessageID: "c@x", ThreadID: "T1", MergedFrom: []string{"T2"}})

	before, _ := Load(path)
	if err := Compact(path, st); err != nil {
		t.Fatal(err)
	}
	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, guid := range []string{"g1", "g2", "g3"} {
		want, _ := before.ThreadOfGUID(guid)
		got, ok := after.ThreadOfGUID(guid)
		if !ok || got != want {
			t.Errorf("after compaction %s reports %q, was %q", guid, got, want)
		}
		// The LIVE state, which is the one the next delivery threads from.
		// Compaction folds the aliases away; a state that dropped them while
		// still pointing at a swallowed thread would place the next reply into
		// a thread that no longer exists -- splitting a conversation that had
		// already been merged, from the tidying step itself.
		if live, ok := st.ThreadOfGUID(guid); !ok || live != want {
			t.Errorf("the live state after compaction reports %q for %s, want %q", live, guid, want)
		}
	}
	if id, ok := after.ThreadOfMessage("b@x"); !ok || id != "T1" {
		t.Errorf("the merged thread's message reports %q/%v, want T1/true", id, ok)
	}
	// The file no longer carries aliases: that is the point.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "\nA\t") || strings.HasPrefix(string(body), "A\t") {
		t.Errorf("compaction left alias records behind:\n%s", body)
	}
}

// Two processes compacting the same state must produce the same bytes, or a
// replicated account differs by who compacted it last -- the rule the
// subscriptions file already follows.
func TestCompactionIsByteIdentical(t *testing.T) {
	build := func() string {
		path := storePath(t)
		st, _ := Load(path)
		mustAppend(t, path, st, Placement{GUID: "g2", MessageID: "b@x", ThreadID: "T2"})
		mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", SubjectKey: "hello", ThreadID: "T1"})
		mustAppend(t, path, st, Placement{GUID: "g3", ThreadID: "T1", MergedFrom: []string{"T2"}})
		if err := Compact(path, st); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if a, b := build(), build(); a != b {
		t.Errorf("two compactions of the same history differ:\n%s\n---\n%s", a, b)
	}
}

// The order records are written in decides which half survives a crash
// mid-append. Whichever tail is lost, the survivors must be the safe ones: an
// alias with no G record names a conversation with one fewer message, while a
// G record with no alias is a merge that did not happen -- two halves of one
// conversation, apart permanently.
func TestTheAliasIsWrittenBeforeTheMessageItMerges(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{GUID: "g1", ThreadID: "T1"})
	mustAppend(t, path, st, Placement{GUID: "g2", ThreadID: "T2"})

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, path, st, Placement{GUID: "g3", MessageID: "late@x", ThreadID: "T1", MergedFrom: []string{"T2"}})
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	appended := string(after[len(before):])
	aliasAt := strings.Index(appended, "A\t")
	guidAt := strings.Index(appended, "G\tg3")
	if aliasAt < 0 || guidAt < 0 {
		t.Fatalf("the append is missing a record:\n%s", appended)
	}
	if aliasAt > guidAt {
		t.Errorf("the G record precedes its alias, so a torn tail loses the merge and splits the thread:\n%s", appended)
	}
}

// A record shape this build does not know is skipped, not refused: a sidecar
// written by a newer version must not stop an older one from threading at all,
// and the worst a skipped record costs is a join.
func TestAnUnknownRecordIsSkipped(t *testing.T) {
	path := storePath(t)
	body := "G\tg1\tT1\nX\tsomething\tnew\nM\ta@x\tT1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatalf("an unknown record made the sidecar unreadable: %v", err)
	}
	if id, ok := st.ThreadOfGUID("g1"); !ok || id != "T1" {
		t.Errorf("the records around it were lost: %q/%v", id, ok)
	}
}

// A tail torn by a crash mid-append is skipped for the same reason, and the
// records before it stand: the log is the account's threading history, and
// losing all of it because the last line is half-written would be the worst
// possible reading of one bad byte.
func TestATornTailIsIgnored(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{GUID: "g1", MessageID: "a@x", ThreadID: "T1"})

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("G\tg2"); err != nil { // no third field, no newline
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("a torn tail made the sidecar unreadable: %v", err)
	}
	if id, ok := reloaded.ThreadOfGUID("g1"); !ok || id != "T1" {
		t.Errorf("the complete records were lost: %q/%v", id, ok)
	}
	if _, ok := reloaded.ThreadOfGUID("g2"); ok {
		t.Error("the torn record was accepted")
	}
}

// A tab inside a value would shift the fields and make one record read as
// another. Flattened rather than escaped, because this file is read by
// position and a Message-ID cannot legally contain one anyway.
func TestATabInAValueCannotShiftTheFields(t *testing.T) {
	path := storePath(t)
	st, _ := Load(path)
	mustAppend(t, path, st, Placement{
		GUID: "g1", MessageID: "a@x", SubjectKey: "hello\tT9\tworld", ThreadID: "T1",
	})
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := reloaded.ThreadOfSubject("hello T9 world"); !ok || id != "T1" {
		t.Errorf("the flattened key reads back as %q/%v, want T1/true", id, ok)
	}
	if _, ok := reloaded.ThreadOfSubject("hello"); ok {
		t.Error("a tab in the value forged a shorter key")
	}
}

func mustAppend(t *testing.T, path string, st *State, p Placement) {
	t.Helper()
	if err := Append(path, st, p); err != nil {
		t.Fatalf("append %s: %v", p.GUID, err)
	}
}
