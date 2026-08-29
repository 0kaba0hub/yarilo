package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A store the reference wrote, walked end to end.
//
// The fixture is built by the test rather than shipped: the store is a
// directory tree, and the pieces of it that matter -- the index, its log, the
// map log, the storage file -- are already in the tree as the dboxref fixtures.
// Assembling them here puts them where a real store keeps them.
func referenceStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	inbox := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	storage := filepath.Join(home, "mdbox", "storage")
	for _, d := range []string{inbox, storage} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct{ from, to string }{
		{"index-inbox.index", filepath.Join(inbox, "dovecot.index")},
		{"index-inbox.log", filepath.Join(inbox, "dovecot.index.log")},
		{"map.log", filepath.Join(storage, "dovecot.map.index.log")},
		{"store-m.1", filepath.Join(storage, "m.1")},
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "storage", "mailbox", "dboxref", "testdata", f.from))
		if err != nil {
			t.Fatalf("read fixture %s: %v", f.from, err)
		}
		if err := os.WriteFile(f.to, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// Every message, with the flags and keywords the reference reports for it.
//
// This is the whole point of reading their index: the stored records carry no
// flags at all, so a migration that only read the store would deliver a
// mailbox where nothing has been read and nothing is marked.
func TestWalkingAReferenceStoreCarriesFlagsAndKeywords(t *testing.T) {
	var got []string
	err := dboxRefWalker{}.Walk(referenceStore(t), func(m sourceMessage) error {
		flags := append([]string(nil), m.Flags...)
		sort.Strings(flags)
		got = append(got, m.Folder+" ["+strings.Join(flags, " ")+"] "+subject(m.Body))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)

	want := []string{
		"INBOX [$HasNoAttachment] msg6",
		"INBOX [$HasNoAttachment \\Seen] msg1",
		"INBOX [$HasNoAttachment \\Answered] msg2",
		"INBOX [$HasNoAttachment $Important] msg3",
	}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("walked %d messages:\n%s\nwant %d:\n%s",
			len(got), strings.Join(got, "\n"), len(want), strings.Join(want, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d is\n  %s\nwant\n  %s", i, got[i], want[i])
		}
	}
}

// The bodies are the messages, not the records around them.
func TestTheBodiesAreWholeMessages(t *testing.T) {
	err := dboxRefWalker{}.Walk(referenceStore(t), func(m sourceMessage) error {
		if len(m.Body) == 0 {
			t.Errorf("%s: empty body", subject(m.Body))
		}
		if !strings.Contains(string(m.Body), "Subject: ") {
			t.Errorf("body carries no Subject header: %q", trimBody(m.Body))
		}
		if strings.HasPrefix(string(m.Body), "\x01\x02") {
			t.Errorf("body starts with the record header, so the header size was not applied")
		}
		if m.GUID == ([16]byte{}) {
			t.Errorf("%s: no guid, so message identity is lost across the migration", subject(m.Body))
		}
		if m.InternalDate.IsZero() {
			t.Errorf("%s: no internal date", subject(m.Body))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func subject(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "Subject: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Subject: "))
		}
	}
	return "?"
}

func trimBody(b []byte) string {
	if len(b) > 60 {
		return string(b[:60]) + "…"
	}
	return string(b)
}

// The flags byte carries bits that are not flags.
//
// None of this can come from the fixture: the store has \Seen, \Answered and a
// keyword, and nothing else. So the mapping for \Deleted and \Draft is
// unexercised by it, and so is the masking -- a reader that copied the byte
// whole would pass every row taken from that store.
//
// The bits above 0x1f are the reference's own: 0x20 is session state, 0x40 is
// MAIL_INDEX_MAIL_FLAG_BACKEND and 0x80 is DIRTY. Carried across they become
// flags nobody set, on a path where nothing notices -- they are not IMAP
// flags, so no client asks for them and no comparison of flag names sees them.
func TestOnlyTheFiveSystemFlagsAreCarried(t *testing.T) {
	for _, tc := range []struct {
		name string
		byte uint8
		want []string
	}{
		{"seen", 0x08, []string{`\Seen`}},
		{"deleted, which the fixture has no message with", 0x04, []string{`\Deleted`}},
		{"draft, likewise", 0x10, []string{`\Draft`}},
		{"all five", 0x1f, []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}},
		{"recent is not carried", 0x20, nil},
		{"the backend bit is not a flag", 0x40, nil},
		{"nor is dirty", 0x80, nil},
		{"a real flag beside the internal ones", 0x08 | 0x40 | 0x80, []string{`\Seen`}},
		{"every bit set", 0xff, []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := flagNames(tc.byte)
			if len(got) != len(tc.want) {
				t.Fatalf("byte %#02x gives %v, want %v", tc.byte, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("byte %#02x gives %v, want %v", tc.byte, got, tc.want)
					break
				}
			}
		})
	}
}

// A folder without an index stops the import; it does not come back empty.
//
// Reading the stored records and placing each message by the folder its
// trailer names is the second branch of #1524 and is not written. Until it is,
// the only safe answer is to refuse: a folder imported as empty is the one
// outcome nobody checks, because the folder is there, the count is zero, and
// nothing in the output says the mail was not read.
func TestAFolderWithoutAnIndexIsRefusedRatherThanImportedEmpty(t *testing.T) {
	home := referenceStore(t)

	// A second folder with messages on disk and no index, which is what a
	// store looks like when its indexes were never copied.
	archive := filepath.Join(home, "mdbox", "mailboxes", "Archive", "dbox-Mails")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}

	var seen int
	err := dboxRefWalker{}.Walk(home, func(sourceMessage) error {
		seen++
		return nil
	})
	if err == nil {
		t.Fatalf("the walk finished after visiting %d messages, and said nothing about the folder it could not read", seen)
	}
	if !strings.Contains(err.Error(), "Archive") {
		t.Errorf("the error does not name the folder: %v", err)
	}
	if !strings.Contains(err.Error(), "no index") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// An index that is there and unreadable is a different error, and also not
// silence: a permission problem on one folder must not look like an empty one.
func TestAnUnreadableIndexIsAnErrorOfItsOwn(t *testing.T) {
	home := referenceStore(t)
	idx := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails", "dovecot.index")
	if err := os.Chmod(idx, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(idx, 0o600) })

	err := dboxRefWalker{}.Walk(home, func(sourceMessage) error { return nil })
	if err == nil {
		t.Fatal("an unreadable index was treated as an absent one, and the folder came back empty")
	}
	if strings.Contains(err.Error(), "no index") {
		t.Errorf("an unreadable index is reported as a missing one: %v", err)
	}
}
