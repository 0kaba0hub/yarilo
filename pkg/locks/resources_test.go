package locks_test

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// The full-text index and the mail index are different resources with
// different writers. Sharing a key made every FTS pass contend with every
// session mail-index write for a guarantee that concerns neither of them
// (#1004), so the keys must not collide for any mailbox — including the ones a
// naive prefix scheme would alias.
func TestFTSKeyNeverCollidesWithMailboxKey(t *testing.T) {
	tests := []struct{ user, folder string }{
		{"u1@example.com", "INBOX"},
		{"u1@example.com", ""},
		{"u1@example.com", "Work/Reports"},
		{"u1@example.com", "mbox:trick"},
		{"", ""},
	}
	for _, tt := range tests {
		fts := locks.FTSKey(tt.user, tt.folder)
		mbox := locks.MailboxKey(tt.user, tt.folder)
		if fts == mbox {
			t.Errorf("FTSKey(%q,%q) == MailboxKey: %q", tt.user, tt.folder, fts)
		}
	}
	// Folders and users must still be distinct from each other, or one user's
	// mailboxes would serialise against each other for no reason.
	if locks.FTSKey("u1", "A") == locks.FTSKey("u1", "B") {
		t.Error("FTSKey does not distinguish folders")
	}
	if locks.FTSKey("u1", "A") == locks.FTSKey("u2", "A") {
		t.Error("FTSKey does not distinguish users")
	}
	// The per-user scope used by optimisation must not alias a real folder.
	if locks.FTSKey("u1", "") == locks.FTSKey("u1", "INBOX") {
		t.Error("the per-user FTS scope aliases a folder")
	}
}
