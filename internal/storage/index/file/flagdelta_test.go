package file

import (
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func flagsOf(t *testing.T, ui *userIndex, folderID uint64, uid uint32) (flags, keywords []string) {
	t.Helper()
	msgs, err := ui.GetMessages(folderID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	for _, m := range msgs {
		if m.UID == uid {
			return m.Flags, m.Keywords
		}
	}
	t.Fatalf("uid %d not in the folder", uid)
	return nil, nil
}

// The window this closes: a caller reads a message, another session changes its
// flags, and the first caller writes back the set it read. With an absolute
// write the second change disappears — the writer is not wrong about \Seen, it
// is wrong about everything else it repeats.
//
// The row asserts BOTH survive. Asserting only that \Seen was set would pass on
// the broken code, which is exactly how this lived in the tree.
func TestAddFlagsKeepsAConcurrentChange(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "alice@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// What a FETCH would have read.
	staleFlags, staleKeywords := flagsOf(t, ui, f.ID, 1)

	// Another session flags it and files it under a keyword.
	if err := ui.AddFlags(f.ID, 1, []string{`\Flagged`}, []string{"$Important"}); err != nil {
		t.Fatalf("concurrent AddFlags: %v", err)
	}

	// The first caller now sets \Seen from what it read.
	if err := ui.AddFlags(f.ID, 1, []string{`\Seen`}, nil); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}

	flags, keywords := flagsOf(t, ui, f.ID, 1)
	for _, want := range []string{`\Seen`, `\Flagged`} {
		if !slices.Contains(flags, want) {
			t.Errorf("flags %v lost %s", flags, want)
		}
	}
	if !slices.Contains(keywords, "$Important") {
		t.Errorf("keywords %v lost $Important", keywords)
	}

	// And the absolute write is what it is: the same sequence through
	// UpdateFlags drops the other session's work. Pinned so the difference
	// between the two calls is a fact in the suite rather than a claim in a
	// comment.
	if err := ui.UpdateFlags(f.ID, 1, append(staleFlags, `\Seen`), staleKeywords); err != nil {
		t.Fatalf("UpdateFlags: %v", err)
	}
	flags, keywords = flagsOf(t, ui, f.ID, 1)
	if slices.Contains(flags, `\Flagged`) || slices.Contains(keywords, "$Important") {
		t.Errorf("UpdateFlags kept %v/%v — it is documented as writing an absolute set", flags, keywords)
	}
}

// RemoveFlags clears the named flag and nothing else, which is what lets POP3
// RSET drop \Seen without repeating a session-old snapshot back into the index.
func TestRemoveFlagsClearsOnlyWhatItNames(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "bob@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 7, Filename: "7", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := ui.AddFlags(f.ID, 7, []string{`\Seen`, `\Flagged`}, []string{"$Work", "$Later"}); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}

	if err := ui.RemoveFlags(f.ID, 7, []string{`\Seen`}, []string{"$Later"}); err != nil {
		t.Fatalf("RemoveFlags: %v", err)
	}

	flags, keywords := flagsOf(t, ui, f.ID, 7)
	if slices.Contains(flags, `\Seen`) {
		t.Errorf("flags %v still carry \\Seen", flags)
	}
	if !slices.Contains(flags, `\Flagged`) {
		t.Errorf("flags %v lost \\Flagged, which was not named", flags)
	}
	if slices.Contains(keywords, "$Later") {
		t.Errorf("keywords %v still carry $Later", keywords)
	}
	if !slices.Contains(keywords, "$Work") {
		t.Errorf("keywords %v lost $Work, which was not named", keywords)
	}
}
