package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// seedAccount lays down a small conversation plus an unrelated message, in the
// shape the backfill has to read: real files, real index, real GUIDs.
func seedAccount(t *testing.T, root, user string) *mailbox.UserInfo {
	t.Helper()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo(user, "")
	mb, idx := maildir.New(), fileindex.New()

	box := mb.OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := idx.OpenUser(info)
	defer ui.Close() //nolint:errcheck

	msgs := []struct {
		folder string
		raw    string
	}{
		{"INBOX", "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"},
		{"INBOX", "Message-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nSubject: Re: Plan\r\n\r\nbody\r\n"},
		{"INBOX", "Message-ID: <other@x>\r\nSubject: Unrelated\r\n\r\nbody\r\n"},
		// In another folder, answering the first: a conversation spans folders,
		// which is why the sidecar and its lock are per account.
		{"Archive", "Message-ID: <late@x>\r\nReferences: <root@x>\r\nSubject: Re: Plan\r\n\r\nbody\r\n"},
	}
	uid := map[string]uint32{}
	for _, m := range msgs {
		if m.folder != "INBOX" {
			_ = box.Create(m.folder)
		}
		uid[m.folder]++
		name, vsize, guid, err := box.Save(m.folder, strings.NewReader(m.raw), uid[m.folder], int64(len(m.raw)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		f, err := ui.OpenFolder(m.folder, 0)
		if err != nil {
			t.Fatalf("open folder: %v", err)
		}
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid[m.folder], Filename: name, Size: uint32(len(m.raw)), VSize: vsize,
			GUID: guid, InternalDate: time.Now(),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	return info
}

func backfillOpts(root string, force bool) threadOpts {
	return threadOpts{
		Driver: "maildir", Root: root, Template: "%d/%n",
		Offline: true, Force: force,
	}
}

// What the backfill is for: an account that predates threading gets its
// conversations, computed from the mail it already has.
func TestBackfillBuildsTheConversations(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")

	if err := runThreadBackfill(backfillOpts(root, false)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatal(err)
	}
	// Three messages in one conversation (across two folders), one on its own.
	if got := st.Threads(); len(got) != 2 {
		t.Fatalf("threads = %v, want 2 (one conversation and one loner)", got)
	}
	var biggest int
	for _, id := range st.Threads() {
		if n := len(st.GUIDsOfThread(id)); n > biggest {
			biggest = n
		}
	}
	if biggest != 3 {
		t.Errorf("largest conversation has %d messages, want 3 -- the reply in Archive did not join", biggest)
	}
}

// The property the whole design leans on. The sidecar is derived state, and
// the argument for having no fsync on the delivery path is that this step can
// rebuild it. A rebuild that produced different thread ids from the same
// history would not be a rebuild -- it would be a second opinion, and every
// client's cached conversation would be wrong after it.
func TestARebuildReproducesTheSameStateByteForByte(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")
	path := threads.PathFor(info)

	if err := runThreadBackfill(backfillOpts(root, false)); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := runThreadBackfill(backfillOpts(root, true)); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("two rebuilds of one mailbox differ:\n%s\n---\n%s", first, second)
	}
}

// The enumeration order must not decide the answer. Which message names a
// conversation depends on which is seen first, so a rebuild that walked the
// mailbox in whatever order the filesystem offered would give different thread
// ids for the same history -- and every client's cached conversation would be
// wrong after a rerun.
//
// Driven by handing the builder the same folders in different orders, because
// a filesystem hands them over in a stable order on one machine: a test that
// only reran the whole tool would pass with the normalisation removed, and did.
func TestTheFolderOrderDoesNotDecideTheAnswer(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")
	mb, idx := maildir.New(), fileindex.New()

	build := func(names []string) string {
		box := mb.OpenUser(info)
		defer box.Close() //nolint:errcheck
		ui := idx.OpenUser(info)
		defer ui.Close() //nolint:errcheck

		path := filepath.Join(t.TempDir(), "threads")
		var st threadStats
		if _, err := buildSidecar(box, ui, names, path, backfillOpts(root, true), info.Username, &st); err != nil {
			t.Fatalf("build: %v", err)
		}
		body, err := os.ReadFile(path + ".rebuild")
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	forward := build([]string{"Archive", "INBOX"})
	backward := build([]string{"INBOX", "Archive"})
	if forward != backward {
		t.Errorf("the folder order changed the result:\n%s\n---\n%s", forward, backward)
	}
}

// A rerun must not rewrite state that deliveries have been extending: the
// rebuild replaces the whole file, so it is asked for rather than assumed.
func TestARerunLeavesAnExistingSidecarAlone(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")
	path := threads.PathFor(info)

	if err := runThreadBackfill(backfillOpts(root, false)); err != nil {
		t.Fatal(err)
	}
	// A delivery after the migration: something the rebuild has never seen.
	st, err := threads.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := threads.Append(path, st, threads.Placement{
		GUID: "later", MessageID: "later@x", ThreadID: "later",
	}); err != nil {
		t.Fatal(err)
	}

	if err := runThreadBackfill(backfillOpts(root, false)); err != nil {
		t.Fatal(err)
	}
	after, err := threads.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.ThreadOfGUID("later"); !ok {
		t.Error("a rerun without --force erased a placement made after the migration")
	}
}

// --dry-run reports and writes nothing, which is what makes it safe to point
// at a live store before choosing a window.
func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")

	o := backfillOpts(root, false)
	o.DryRun = true
	if err := runThreadBackfill(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(threads.PathFor(info)); !os.IsNotExist(err) {
		t.Errorf("dry run left a file: %v", err)
	}
}

// Every account under the root, not just the one that happens to sort first.
func TestBackfillWalksEveryAccount(t *testing.T) {
	root := t.TempDir()
	infos := []*mailbox.UserInfo{
		seedAccount(t, root, "alice@example.com"),
		seedAccount(t, root, "bob@example.com"),
	}
	if err := runThreadBackfill(backfillOpts(root, false)); err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if _, err := os.Stat(threads.PathFor(info)); err != nil {
			t.Errorf("%s has no sidecar: %v", info.Username, err)
		}
	}
}

// A message with no GUID cannot be keyed, and threading it under the zero id
// would put every such message in one conversation. It is skipped, counted and
// named -- the GUID backfill is the step that fixes it.
func TestMessagesWithoutAGuidAreSkippedNotMerged(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")
	// Strip the GUIDs from the index the crude way: rewrite the account with a
	// second message whose meta carries none.
	idx := fileindex.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	box := maildir.New().OpenUser(info)
	raw := "Message-ID: <noguid@x>\r\nSubject: Old\r\n\r\nbody\r\n"
	name, vsize, _, err := box.Save("INBOX", strings.NewReader(raw), 99, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 99, Filename: name, Size: uint32(len(raw)), VSize: vsize,
	}); err != nil {
		t.Fatal(err)
	}
	idx.Close() //nolint:errcheck
	box.Close() //nolint:errcheck

	if err := runThreadBackfill(backfillOpts(root, true)); err != nil {
		t.Fatal(err)
	}
	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ThreadOfGUID(""); ok {
		t.Error("a message with no GUID was threaded under the empty id")
	}
	if got := st.Threads(); len(got) != 2 {
		t.Errorf("threads = %v, want the same 2 as before the unkeyed message", got)
	}
}

func TestUnreadableMessagesDoNotStopTheRun(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")

	// Take a message's file away, leaving its index entry behind.
	matches, err := filepath.Glob(filepath.Join(info.Home, "Maildir", "cur", "*"))
	if err != nil || len(matches) == 0 {
		t.Skipf("cannot find a delivered file to remove: %v (%d)", err, len(matches))
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}

	if err := runThreadBackfill(backfillOpts(root, true)); err != nil {
		t.Fatalf("one unreadable message stopped the whole run: %v", err)
	}
	if _, err := os.Stat(threads.PathFor(info)); err != nil {
		t.Errorf("no sidecar was written: %v", err)
	}
	fmt.Fprintln(os.Stderr) // keep the log tail readable in -v runs
}
