package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/locks"
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
		if _, err := buildSidecar(box, ui, names, path, info.Username, &st); err != nil {
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

// A dry run exists to give an operator numbers before they choose a window, so
// it has to measure the mode the real run uses. It did not: skipping the append
// also skipped the only thing that applies a placement to the state, so every
// later message saw "nothing to join" and any mailbox reported zero
// conversations. A probe that cannot say which mode it measured in is the trap
// we have hit three times already.
func TestDryRunReportsTheNumbersARealRunWouldProduce(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")

	dry := backfillOpts(root, false)
	dry.DryRun = true
	var dryStats threadStats
	if err := runThreadBackfillInto(dry, &dryStats); err != nil {
		t.Fatal(err)
	}

	var realStats threadStats
	if err := runThreadBackfillInto(backfillOpts(root, false), &realStats); err != nil {
		t.Fatal(err)
	}

	if dryStats.Threads != realStats.Threads || dryStats.Messages != realStats.Messages {
		t.Errorf("dry run reported %d threads over %d messages; the real run produced %d over %d",
			dryStats.Threads, dryStats.Messages, realStats.Threads, realStats.Messages)
	}
	if realStats.Threads == 0 {
		t.Fatal("the real run found no conversations, so this comparison proves nothing")
	}
	// And it still leaves nothing behind, working file included.
	if _, err := os.Stat(threads.PathFor(info) + ".rebuild"); !os.IsNotExist(err) {
		t.Errorf("the dry run left its working file: %v", err)
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

// recordingLocker satisfies locks.Locker and remembers what was held, so a
// test can assert on the lock rather than on the code that takes it.
type recordingLocker struct {
	held    []string
	holding map[string]bool
}

func newRecordingLocker() *recordingLocker {
	return &recordingLocker{holding: map[string]bool{}}
}

func (l *recordingLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.held = append(l.held, resource)
	l.holding[resource] = true
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *recordingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}
func (l *recordingLocker) Unlock(_ context.Context, lockID string) error {
	delete(l.holding, lockID)
	return nil
}
func (l *recordingLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *recordingLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *recordingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *recordingLocker) HoldsResource(resource string) bool                          { return l.holding[resource] }
func (l *recordingLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (l *recordingLocker) Close() error { return nil }

// The rebuild writes into shared storage, so it goes under the account's
// thread lock -- all of it, not just the install.
//
// The walk reads every message's headers and takes as long as the account is
// big. A delivery landing during it appends to the live sidecar; installing a
// file built before that delivery would erase it. Between runs that is what
// --force guards, but inside one run only the lock does.
func TestTheRebuildHoldsTheAccountLock(t *testing.T) {
	root := t.TempDir()
	info := seedAccount(t, root, "alice@example.com")
	locker := newRecordingLocker()

	var st threadStats
	o := backfillOpts(root, false)
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	resolveUser := func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil }
	if err := threadUser(maildir.New(), nil, fileindex.New(), resolveUser,
		locker, o, info.Username, &st); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	want := locks.ThreadsKey(info.Username)
	var found bool
	for _, r := range locker.held {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("locks taken = %v, want the account's thread lock %q -- the rebuild wrote into shared storage without it",
			locker.held, want)
	}
	if locker.holding[want] {
		t.Error("the thread lock was not released when the account finished")
	}
}

// An account whose driver is not the run's default must be read with its own
// driver, and its sidecar written where the deliveries write it.
//
// This is #1456. The backfill resolved every account through the resolver's
// defaults, so a userdb account with `mbtype=mdbox` was opened as maildir --
// "open .../Maildir: no such file" on mdbox and sdbox accounts, and on maildir
// accounts a sidecar written to the home directory while the deliveries kept
// extending the real one a level below. Three of the QA window's five numbers
// are taken with this command; all three were unobtainable.
//
// The row seeds the mail under a per-driver root, exactly as userdb hands it
// out, and asserts on the path the sidecar lands at -- the observable half of
// the defect. With the account resolved through defaults, nothing is written
// there at all.
func TestBackfillFollowsTheAccountsOwnDriverAndMailRoot(t *testing.T) {
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	base := resolver.UserInfo("u1@d00001.test", "")

	// What userdb gives a non-default account: its own driver, and a mail root
	// one level below the home directory.
	info := *base
	info.Driver = "mdbox"
	info.MailPath = filepath.Join(base.Home, "mdbox")

	box := maildir.New().OpenUser(&info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	idx := fileindex.New().OpenUser(&info)
	raw := "Message-ID: <a@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: uint32(len(raw)), VSize: vsize,
		GUID: guid, InternalDate: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	idx.Close() //nolint:errcheck
	box.Close() //nolint:errcheck

	var askedFor []string
	byDriver := func(d string) mailbox.MailboxBackend {
		askedFor = append(askedFor, d)
		return maildir.New()
	}
	resolveUser := func(string) (*mailbox.UserInfo, error) { return &info, nil }

	var st threadStats
	if err := threadUser(maildir.New(), byDriver, fileindex.New(), resolveUser,
		nil, backfillOpts(root, false), info.Username, &st); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// The account's driver decided the storage.
	if len(askedFor) != 1 || askedFor[0] != "mdbox" {
		t.Errorf("backends asked for %v, want exactly [mdbox] -- the account's own driver", askedFor)
	}
	// And the sidecar is where the deliveries keep theirs: in the mail root,
	// not in the home directory above it.
	want := filepath.Join(info.MailPath, threads.FileName)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("no sidecar at %s: %v", want, err)
	}
	if stray := filepath.Join(base.Home, threads.FileName); stray != want {
		if _, err := os.Stat(stray); err == nil {
			t.Errorf("a second sidecar was written to the home directory at %s -- deliveries would never read it", stray)
		}
	}
	if st.Threads != 1 {
		t.Errorf("threads = %d, want 1", st.Threads)
	}
}
