package pop3

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

type namedBackend struct{ name string }

func (n *namedBackend) OpenUser(*mailbox.UserInfo) mailbox.UserMailbox { return nil }

func TestResolvePersonalBox(t *testing.T) {
	global := &namedBackend{name: "global-maildir"}
	sdbox := &namedBackend{name: "sdbox"}
	byDriver := func(driver string) mailbox.MailboxBackend {
		if driver == "sdbox" {
			return sdbox
		}
		return nil // unknown driver → caller falls back to global
	}

	cases := []struct {
		name       string
		byDriver   func(string) mailbox.MailboxBackend
		mailLoc    string
		wantBox    *namedBackend
		wantDriver string
	}{
		{"no mailloc uses global", byDriver, "", global, ""},
		{"no driver prefix uses global", byDriver, "/var/mail/%u", global, ""},
		{"sdbox driver selects per-user backend", byDriver, "sdbox:~/sdbox", sdbox, "sdbox"},
		{"driver parsed even when factory nil", nil, "sdbox:~/sdbox", global, "sdbox"},
		{"unknown driver falls back to global but keeps name", byDriver, "mdbox:~/mdbox", global, "mdbox"},
		{"driver lowercased", byDriver, "SDBOX:~/x", sdbox, "sdbox"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			box, driver := resolvePersonalBox(global, c.byDriver, c.mailLoc)
			if box != c.wantBox {
				t.Errorf("box = %v, want %v", box.(*namedBackend).name, c.wantBox.name)
			}
			if driver != c.wantDriver {
				t.Errorf("driver = %q, want %q", driver, c.wantDriver)
			}
		})
	}
}
