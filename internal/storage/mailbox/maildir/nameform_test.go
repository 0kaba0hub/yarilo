package maildir

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// folderDiskName is form-preserving and round-trips, including a combining mark
// straight after the escape character — the input that broke every earlier
// version of this code.
//
// The reason it now just works: the driver no longer normalises. NFC runs once
// at the name-entry boundary (mailbox.NormalizeName), before the name reaches
// here, so escaping never has an NFC pass after it to compose a combining mark
// into the escape's hex ("^" -> "^5e", and a following mark stays a separate
// rune instead of merging with the "e"). The ordering question that #1078,
// #1092 and #1113 each got wrong no longer exists, because there is nothing to
// order — one owner, upstream (#1113).
func TestFolderDiskNameRoundTripsEveryForm(t *testing.T) {
	names := []string{
		"^́x",         // combining mark straight after the escape character
		"a.b́c",       // and after an escaped layout separator
		"Café",       // a combining mark, no escape nearby
		"Café",        // its composed form
		"a^b",         // the escape character with nothing after it
		"Invoices.20", // a literal layout separator
	}

	for _, listUTF8 := range []bool{true, false} {
		b := New(WithListUTF8(listUTF8))
		u := b.OpenUser(&mailbox.UserInfo{
			Username: "u@test", Home: "/srv/u", MailPath: "/srv/u", Separator: "/",
			StorageEscapeChar: "^",
		}).(*userMailbox)

		for _, name := range names {
			disk := u.folderDiskName(name)
			logical := disk
			if !listUTF8 {
				decoded, err := fromModUTF7(disk)
				if err != nil {
					t.Fatalf("utf8=%v %q: decode %q: %v", listUTF8, name, disk, err)
				}
				logical = decoded
			}
			logical = mailbox.UnescapeStorageName(logical, "^")
			if logical != name {
				t.Errorf("utf8=%v: %q stored as %q, read back as %q — not form-preserving", listUTF8, name, disk, logical)
			}
		}
	}
}
