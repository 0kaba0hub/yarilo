package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// deliverToCur writes a file straight into cur/ with its ":2," trailer already
// set, the way a store being taken over holds it.
func deliverToCur(t *testing.T, box *userMailbox, name, body string) {
	t.Helper()
	p := filepath.Join(box.folderPath("INBOX"), "cur", name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("deliver to cur: %v", err)
	}
}

func writeKeywordFile(t *testing.T, box *userMailbox, content string) {
	t.Helper()
	p := filepath.Join(box.folderPath("INBOX"), keywordsFileName)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write keyword file: %v", err)
	}
}

func curName(t *testing.T, box *userMailbox, base string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(box.folderPath("INBOX"), "cur"))
	if err != nil {
		t.Fatalf("read cur: %v", err)
	}
	for _, e := range entries {
		if maildirBase(e.Name()) == base {
			return e.Name()
		}
	}
	t.Fatalf("no file in cur/ with base %q", base)
	return ""
}

// The index is what a client reads, so the index is what a test asks.
//
// Scan used to merge keywords into ScanRecord.Flags, and ReconcileIndex passed
// that merged list on as flags with no keywords beside it: every keyword an
// adopted store held was dropped on the way into the index (#1605). Reading the
// scanner back proves nothing here -- it was the seam that lost them, not the
// reader.
func TestReconcile_KeywordsReachTheIndexUnderTheirRealNames(t *testing.T) {
	box, idx, folder := recSetup(t)
	writeKeywordFile(t, box, "0 $HasNoAttachment\n1 $Important\n3 $Label\n")
	const base = "1700000010.M1P1.host,S=20"
	deliverToCur(t, box, base+":2,abdS", "From: a@b\r\n\r\nx\r\n")

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}

	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("index holds %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{"$HasNoAttachment", "$Important", "$Label"} {
		if !hasString(m.Keywords, want) {
			t.Errorf("index keywords are %v, and the keyword file names %s on this message", m.Keywords, want)
		}
	}
	if !hasString(m.Flags, `\Seen`) {
		t.Errorf("index flags are %v, want \\Seen", m.Flags)
	}
	if hasString(m.Flags, "$Important") {
		t.Errorf("keywords landed in Flags: %v", m.Flags)
	}
}

// The half that damages the store.
//
// A flag write serialises the index's whole view back into the filename. With
// the keywords lost on adoption that view was empty, so the first STORE after a
// takeover stripped the keyword letters off the file -- and once they are gone
// the original state cannot be read back from the store at all (#1605).
func TestReconcile_AStoreAfterAdoptionKeepsTheKeywordLetters(t *testing.T) {
	box, idx, folder := recSetup(t)
	writeKeywordFile(t, box, "0 $HasNoAttachment\n1 $Important\n")
	const base = "1700000011.M1P1.host,S=20"
	deliverToCur(t, box, base+":2,ab", "From: a@b\r\n\r\nx\r\n")

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("index = %v, err = %v", msgs, err)
	}
	m := msgs[0]

	// What a STORE +FLAGS (\Seen) does: the index resolves the new set, the
	// driver writes that set to the filename.
	res, err := idx.UpdateFlagsMulti(folder.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Flags: []string{`\Seen`}, Mode: mailbox.FlagsAdd},
	})
	if err != nil {
		t.Fatalf("update flags: %v", err)
	}
	got := res[m.UID]
	if _, err := box.WriteFlags("INBOX", m.Filename, got.Flags, got.Keywords); err != nil {
		t.Fatalf("write flags: %v", err)
	}

	name := curName(t, box, base)
	info := name[strings.Index(name, ":2,")+3:]
	if !strings.Contains(info, "S") {
		t.Errorf("filename is %q, want the \\Seen letter S", name)
	}
	for _, letter := range []string{"a", "b"} {
		if !strings.Contains(info, letter) {
			t.Errorf("filename is %q: keyword letter %q was on disk before the store and is gone", name, letter)
		}
	}
}

func hasString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
