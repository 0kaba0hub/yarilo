package maildir

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// NFC and escaping do not commute, and the order is not free.
//
// Escaping emits ASCII hex, so a combining mark that follows an escaped byte
// composes with one of the hex digits: normalising after escaping turns
// "^" U+0301 "x" into "^5" é "x" and the escape sequence is gone. The name then
// cannot be read back -- LIST returns something the client never wrote -- and
// the mail tree spells it differently from every derived tree, which is the
// divergence #1092 exists to remove, surviving in one corner.
//
// #1078 required escaping before modUTF7, because escaping an already-base64
// name is safe only while the escape character stays outside that alphabet.
// That constraint says nothing about NFC, and this order keeps it.
func TestFolderNameSurvivesACombiningMarkAfterAnEscape(t *testing.T) {
	names := []string{
		"^\u0301x",    // combining mark straight after the escape character
		"a.b\u0301c",  // and after an escaped layout separator
		"Cafe\u0301",  // ordinary combining mark, no escape nearby
		"a^b",         // the escape character with nothing after it
		"Invoices.20", // a literal layout separator, no marks
	}

	for _, listUTF8 := range []bool{true, false} {
		b := New(WithNormalizeNFC(true), WithListUTF8(listUTF8))
		u := b.OpenUser(&mailbox.UserInfo{
			Username: "u@test", Home: "/srv/u", MailPath: "/srv/u", Separator: "/",
			StorageEscapeChar: "^",
		}).(*userMailbox)

		for _, name := range names {
			want := nfcNormalize(name)
			disk := u.folderDiskName(name)

			// Read back the way ListFolders does.
			logical := disk
			if !listUTF8 {
				decoded, err := fromModUTF7(disk)
				if err != nil {
					t.Fatalf("utf8=%v %q: decode %q: %v", listUTF8, name, disk, err)
				}
				logical = decoded
			}
			logical = mailbox.UnescapeStorageName(logical, "^")
			if got := nfcNormalize(logical); got != want {
				t.Errorf("utf8=%v: %q stored as %q, read back as %q", listUTF8, name, disk, got)
			}
		}
	}
}
