package mailbox

import "testing"

func TestFolderSubpath(t *testing.T) {
	cases := []struct {
		driver, folder, disk, sep, want string
	}{
		// maildir: flat maildir++ layout, "." separates hierarchy.
		{"maildir", "INBOX", "INBOX", ".", ""},
		{"maildir", "Sent", "Sent", ".", ".Sent"},
		{"maildir", "ProjZ.Sub", "ProjZ.Sub", ".", ".ProjZ.Sub"},
		{"", "ProjZ.Sub", "ProjZ.Sub", ".", ".ProjZ.Sub"}, // empty driver → maildir
		// maildir with "/" IMAP sep still flattens to dotted on disk.
		{"maildir", "ProjZ/Sub", "ProjZ/Sub", "/", ".ProjZ.Sub"},
		// mdbox: nested, "/"-joined.
		{"mdbox", "INBOX", "INBOX", ".", "mailboxes/INBOX"},
		{"mdbox", "ProjZ.Sub", "ProjZ.Sub", ".", "mailboxes/ProjZ/Sub"},
		{"mdbox", "ProjZ/Sub", "ProjZ/Sub", "/", "mailboxes/ProjZ/Sub"},
		// sdbox: nested + dbox-Mails.
		{"sdbox", "ProjZ.Sub", "ProjZ.Sub", ".", "mailboxes/ProjZ/Sub/dbox-Mails"},
		{"dbox", "Sent", "Sent", ".", "mailboxes/Sent/dbox-Mails"},
	}
	for _, c := range cases {
		if got := FolderSubpath(c.driver, c.folder, c.disk, c.sep); got != c.want {
			t.Errorf("FolderSubpath(%q,%q,%q,sep=%q)=%q, want %q",
				c.driver, c.folder, c.disk, c.sep, got, c.want)
		}
	}
}
