package mailbox

import "testing"

func TestFolderSubpath(t *testing.T) {
	cases := []struct {
		driver, folder, disk, want string
	}{
		{"maildir", "INBOX", "INBOX", "INBOX"},
		{"maildir", "Sent", "Sent", ".Sent"},
		{"", "Sent", "Sent", ".Sent"}, // empty driver defaults to maildir
		{"mdbox", "INBOX", "INBOX", "mailboxes/INBOX"},
		{"mdbox", "Sent", "Sent", "mailboxes/Sent"},
		{"sdbox", "INBOX", "INBOX", "mailboxes/INBOX/dbox-Mails"},
		{"sdbox", "Sent", "Sent", "mailboxes/Sent/dbox-Mails"},
		{"dbox", "Sent", "Sent", "mailboxes/Sent/dbox-Mails"},
	}
	for _, c := range cases {
		if got := FolderSubpath(c.driver, c.folder, c.disk); got != c.want {
			t.Errorf("FolderSubpath(%q,%q,%q)=%q, want %q", c.driver, c.folder, c.disk, got, c.want)
		}
	}
}
