package maildir

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// timingField reads one number out of the last "flags timing" line.
func timingField(out, field string) (int64, bool) {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "flags timing") {
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

// The rename clock spans the renames.
//
// One acquisition covers the whole batch (#1623), so a slow WriteFlagsMulti is
// the wait, the keyword file, or the renames -- and the summed number said only
// that it was slow. A fast disk reports zero for the renames whether the clock
// is around the loop or beside it, which is how two earlier splits measured a
// window without the thing in it (#1648, #1651), so the writes are made slow on
// purpose here (#1662).
func TestTheRenameClockSpansTheRenames(t *testing.T) {
	box, idx, folder := recSetupLocked(t)
	names := []string{"1700000001.M1Pa.host", "1700000002.M1Pb.host", "1700000003.M1Pc.host"}
	for _, n := range names {
		deliverToNew(t, box, n, "From: a@b\r\n\r\nx\r\n")
	}
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(names) {
		t.Fatalf("index holds %d messages, want %d", len(msgs), len(names))
	}

	const each = 25 * time.Millisecond
	beforeFlagRename = func() { time.Sleep(each) }
	defer func() { beforeFlagRename = nil }()

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	writes := make([]mailbox.FlagWrite, 0, len(msgs))
	for _, m := range msgs {
		writes = append(writes, mailbox.FlagWrite{UID: m.UID, Filename: m.Filename, Flags: []string{`\Seen`}})
	}
	for i, res := range box.WriteFlagsMulti("INBOX", writes) {
		if res.Err != nil {
			t.Fatalf("write %d: %v", i, res.Err)
		}
	}

	out := logged.String()
	got, ok := timingField(out, "renames_ms")
	if !ok {
		t.Fatalf("no renames_ms in the timing line:\n%s", out)
	}
	if want := int64(len(writes)) * each.Milliseconds(); got < want {
		t.Errorf("renames_ms = %d for %d renames that slept %dms each: the clock does not span them",
			got, len(writes), each.Milliseconds())
	}
	// The parts are worth nothing if the batch renamed nothing: a call that
	// changed no name also spends no time renaming.
	if n, ok := timingField(out, "renamed"); !ok || n != int64(len(writes)) {
		t.Errorf("renamed = %d, want %d -- the split is timing a batch that did no work", n, len(writes))
	}
	for _, field := range []string{"lock_ms", "keywords_ms", "total_ms"} {
		if _, ok := timingField(out, field); !ok {
			t.Errorf("the timing line has no %s, so the parts cannot be told apart:\n%s", field, out)
		}
	}
}
