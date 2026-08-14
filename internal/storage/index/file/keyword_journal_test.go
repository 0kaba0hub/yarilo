package file

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The mirror of TestFlagOnlyStoreDoesNotRewriteTheBase: with keywords in the
// log, a keyword STORE stops paying the base rewrite too. This is the row that
// records the price being gone -- without it, the rewrite could come back and
// everything else would still pass.
func TestKeywordStoreDoesNotRewriteTheBase(t *testing.T) {
	root := t.TempDir()
	idx := New().OpenUser(&mailbox.UserInfo{Username: "u@x.com", Home: root})
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	base := filepath.Join(root, "yarilo.index")
	logPath := base + ".log"
	before, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	logBefore, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}

	// A keyword the registry has never seen: the case that used to rewrite the
	// base twice over, once for the bit and once for the new name.
	if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsAdd, Keywords: []string{"$Fresh"}},
	}); err != nil {
		t.Fatalf("update flags multi: %v", err)
	}

	after, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("a keyword STORE rewrote the base; the journal is not carrying the keyword")
	}
	logAfter, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log after: %v", err)
	}
	if logAfter.Size() <= logBefore.Size() {
		t.Error("the log did not grow; the keyword was written to memory alone, which is #1278")
	}
}

// Replay order against the flag and modseq records: a fresh descriptor reading
// the tail must land on the state the writer holds in memory -- bits, names and
// modseq together. Asserting only the bit would pass on an index whose registry
// replay is broken, because a bit with no name decodes to nothing.
func TestKeywordJournalReplayMatchesTheWriter(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Interleaved on purpose: system flags and keywords in one batch, then a
	// removal, so replay has to honour the order rather than the record type.
	steps := []mailbox.FlagsUpdate{
		{Mode: mailbox.FlagsAdd, Flags: []string{`\Seen`}, Keywords: []string{"$Work", "$Urgent"}},
		{Mode: mailbox.FlagsAdd, Flags: []string{`\Flagged`}, Keywords: []string{"$Later"}},
		{Mode: mailbox.FlagsRemove, Keywords: []string{"$Urgent"}},
	}
	var want mailbox.FlagsResult
	for i, upd := range steps {
		res, updErr := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{m.UID: upd})
		if updErr != nil {
			t.Fatalf("step %d: %v", i, updErr)
		}
		want = res[m.UID]
	}

	// A second descriptor over the same files: it has only the base and the
	// log, which is exactly what another process has.
	other := New().OpenUser(info)
	defer other.Close() //nolint:errcheck
	f2, err := other.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	msgs, err := other.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("second descriptor sees %d messages, want 1", len(msgs))
	}
	got := msgs[0]

	if !reflect.DeepEqual(sortedCopy(got.Keywords), sortedCopy(want.Keywords)) {
		t.Errorf("replayed keywords = %v, writer holds %v", got.Keywords, want.Keywords)
	}
	if !reflect.DeepEqual(sortedCopy(got.Flags), sortedCopy(want.Flags)) {
		t.Errorf("replayed flags = %v, writer holds %v", got.Flags, want.Flags)
	}
	if got.ModSeq != want.ModSeq {
		t.Errorf("replayed modseq = %d, writer holds %d", got.ModSeq, want.ModSeq)
	}
}

// Clearing every keyword is journalled as one RESET rather than one removal per
// name, and a fresh descriptor has to read it back as an empty set -- not as
// "no records, so nothing changed".
func TestKeywordResetIsJournalledAndReplayed(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsAdd, Keywords: []string{"$A", "$B", "$C"}},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsSet, Keywords: nil},
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if n := countLogRecords(t, filepath.Join(root, "yarilo.index.log"), mailindex.TxTypeKeywordReset); n != 1 {
		t.Errorf("clearing three keywords wrote %d RESET records, want exactly 1", n)
	}

	other := New().OpenUser(info)
	defer other.Close() //nolint:errcheck
	f2, err := other.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	msgs, err := other.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Keywords) != 0 {
		t.Errorf("second descriptor sees keywords %v after a reset, want none", msgs[0].Keywords)
	}
}

// The bit a name gets is allocated in first-seen order, so two descriptors that
// each invent a keyword first can hand the same bit to different names. The
// journal cures it by carrying the name: the reader recomputes the bit rather
// than trusting the number.
func TestKeywordBitsAreNotPortableButNamesAre(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	// A reader that already holds a registry of its own, with the opposite
	// order -- the shape a second process arrives in.
	other := New().OpenUser(info)
	defer other.Close() //nolint:errcheck
	f2, err := other.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := other.AllocateAndAppend(f2.ID, &mailbox.MessageMeta{Filename: "g", Size: 10}); err != nil {
		t.Fatalf("second append: %v", err)
	}
	if _, err := other.UpdateFlagsMulti(f2.ID, map[uint32]mailbox.FlagsUpdate{
		2: {Mode: mailbox.FlagsAdd, Keywords: []string{"$Second"}},
	}); err != nil {
		t.Fatalf("second store: %v", err)
	}

	if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsAdd, Keywords: []string{"$First"}},
	}); err != nil {
		t.Fatalf("first store: %v", err)
	}

	third := New().OpenUser(info)
	defer third.Close() //nolint:errcheck
	f3, err := third.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	msgs, err := third.GetMessages(f3.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	byUID := map[uint32][]string{}
	for _, msg := range msgs {
		byUID[msg.UID] = msg.Keywords
	}
	for uid, want := range map[uint32]string{1: "$First", 2: "$Second"} {
		got := byUID[uid]
		if len(got) != 1 || got[0] != want {
			t.Errorf("uid %d has keywords %v, want [%s] -- a bit was read as a number, not resolved from its name", uid, got, want)
		}
	}
}

// countLogRecords walks a log counting records of one kind.
func countLogRecords(t *testing.T, logPath string, kind mailindex.TxType) int {
	t.Helper()
	buf, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	n := 0
	for off := mailindex.LogHeaderSize; off+8 <= len(buf); {
		hdr, hdrErr := mailindex.DecodeTxHeader(buf[off : off+8])
		if hdrErr != nil || hdr.Size < 8 {
			break
		}
		if hdr.Type.Kind() == kind {
			n++
		}
		off += int(hdr.Size)
	}
	return n
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
