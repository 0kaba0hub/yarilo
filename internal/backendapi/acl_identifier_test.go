package backendapi

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The admin API is the third identifier parser; every form must route through
// the shared one, or a fix lands in two of three places (#1140 review). The
// distinguishing inputs are the ones a direct construction would accept: a
// control character in a bare name, and the reference's anonymous spelling.
func TestParseAdminIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    mailbox.Identifier
		wantErr bool
	}{
		{"bare name", "bob", mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, false},
		{"bare name with space", "John Smith", mailbox.Identifier{Type: mailbox.IDUser, Name: "John Smith"}, false},
		{"prefixed user", "user=alice", mailbox.Identifier{Type: mailbox.IDUser, Name: "alice"}, false},
		{"group", "group=staff", mailbox.Identifier{Type: mailbox.IDGroup, Name: "staff"}, false},
		{"anyone", "anyone", mailbox.Identifier{Type: mailbox.IDAnyone}, false},
		{"anonymous is anyone", "anonymous", mailbox.Identifier{Type: mailbox.IDAnyone}, false},
		{"authenticated", "authenticated", mailbox.Identifier{Type: mailbox.IDAuthenticated}, false},
		{"owner", "owner", mailbox.Identifier{Type: mailbox.IDOwner}, false},
		{"empty", "", mailbox.Identifier{}, true},
		{"empty prefixed", "user=", mailbox.Identifier{}, true},
		// The forged-entry shape: a newline in a bare name would end the
		// line early and start a second entry.
		{"newline in bare name", "bob\nanyone lrswipkxtea", mailbox.Identifier{}, true},
		{"control char in bare name", "bo\x01b", mailbox.Identifier{}, true},
		{"invalid utf8 in bare name", "b\xffb", mailbox.Identifier{}, true},
		{"overlong bare name", strings.Repeat("a", 1030), mailbox.Identifier{}, true},
	}
	for _, c := range cases {
		got, err := parseAdminIdentifier(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: parseAdminIdentifier(%q) accepted, want error", c.name, c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
