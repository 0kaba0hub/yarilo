package file

import (
	"bytes"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A whole command's renames cost one acquisition, not one each.
//
// On maildir a flag change renames the file, so a STORE over 40 messages
// recorded 40 new names and took the index's exclusive lock 40 times. Measured
// at 121ms per name on a contended folder, which is how a single STORE reached
// 18 seconds while every other part of it stayed in the tens of milliseconds
// (#1646).
func TestRecordingAWholeBatchOfNamesTakesOneLock(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)
	const user = "batch@example.com"
	ui := New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	names := make(map[uint32]string, n)
	for uid := uint32(1); uid <= n; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10, VSize: 10,
		}); err != nil {
			t.Fatal(err)
		}
		names[uid] = fmt.Sprintf("%d:2,S", uid)
	}

	before := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite)
	if err := ui.UpdateFilenames(f.ID, names); err != nil {
		t.Fatalf("UpdateFilenames: %v", err)
	}
	if got := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite) - before; got != 1 {
		t.Errorf("recording %d names took %v exclusive acquisitions, want 1", n, got)
	}

	// The zeros above are worth nothing unless every name actually landed: a
	// call that recorded nothing also takes one lock.
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("index holds %d messages, want %d", len(msgs), n)
	}
	for _, m := range msgs {
		if want := names[m.UID]; m.Filename != want {
			t.Errorf("uid %d is named %q, want %q -- the batch reported success without writing",
				m.UID, m.Filename, want)
		}
	}
}

// A name the folder does not carry is skipped, and the rest still land: the
// batch must not be all-or-nothing on a uid another session expunged between
// the rename and the record.
func TestABatchSkipsAnUnknownUIDAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	const user = "batch2@example.com"
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 3; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	err = ui.UpdateFilenames(f.ID, map[uint32]string{
		1: "1:2,S", 99: "99:2,S", 3: "3:2,S",
	})
	if err != nil {
		t.Fatalf("a batch with one unknown uid failed outright: %v", err)
	}
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint32]string{}
	for _, m := range msgs {
		got[m.UID] = m.Filename
	}
	for uid, want := range map[uint32]string{1: "1:2,S", 2: "2:2,", 3: "3:2,S"} {
		if got[uid] != want {
			t.Errorf("uid %d is named %q, want %q", uid, got[uid], want)
		}
	}
}

// The three parts of a name batch are timed apart, and each one is reachable.
//
// Splitting is the whole point: a single name cost 4,013ms in a run whose mean
// was 14.2ms, and "names_ms" cannot say whether that was the wait, the freshness
// check or the write (#1650). A line that reports one number, or that leaves a
// part at zero because its clock never spans it, answers nothing.
func TestANameBatchTimesItsThreePartsApart(t *testing.T) {
	dir := t.TempDir()
	const user = "timed@example.com"
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[uint32]string{}
	for uid := uint32(1); uid <= 8; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
		names[uid] = fmt.Sprintf("%d:2,S", uid)
	}

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	if err := ui.UpdateFilenames(f.ID, names); err != nil {
		t.Fatal(err)
	}

	line := ""
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "names timing") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("no names timing line was logged")
	}
	for _, field := range []string{"lock_ms", "reload_ms", "append_ms", "total_ms", "names"} {
		if !strings.Contains(line, `"`+field+`"`) {
			t.Errorf("the timing line has no %s: the parts cannot be told apart\n%s", field, line)
		}
	}
	if !strings.Contains(line, `"names":8`) {
		t.Errorf("the line reports a different batch size than the eight names given\n%s", line)
	}
}

// The append clock spans the writes. A fast disk reports zero whether the clock
// is around the loop or beside it, so the writes are made slow on purpose: this
// is the part with a known cure, and a split that cannot see it is the #1648
// mistake made a second time.
func TestTheAppendClockSpansTheWrites(t *testing.T) {
	dir := t.TempDir()
	const user = "timed2@example.com"
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[uint32]string{}
	for uid := uint32(1); uid <= 4; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
		names[uid] = fmt.Sprintf("%d:2,S", uid)
	}

	const each = 25 * time.Millisecond
	beforeAppendName = func() { time.Sleep(each) }
	defer func() { beforeAppendName = nil }()

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	if err := ui.UpdateFilenames(f.ID, names); err != nil {
		t.Fatal(err)
	}
	got, ok := timingField(logged.String(), "append_ms")
	if !ok {
		t.Fatal("no append_ms in the timing line")
	}
	if want := int64(len(names)) * each.Milliseconds(); got < want {
		t.Errorf("append_ms = %d for four writes that slept %dms each: the clock does not span them",
			got, each.Milliseconds())
	}
}

// timingField reads one number out of the last "names timing" line.
func timingField(out, field string) (int64, bool) {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "names timing") {
			line = l
		}
	}
	at := strings.Index(line, `"`+field+`":`)
	if at < 0 {
		return 0, false
	}
	rest := line[at+len(field)+3:]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest[:end]), 10, 64)
	return n, err == nil
}
