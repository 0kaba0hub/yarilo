package pop3

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

type namedBackend struct{ name string }

func (n *namedBackend) OpenUser(*mailbox.UserInfo) mailbox.UserMailbox { return nil }

func TestResolvePersonalStorage(t *testing.T) {
	global := &namedBackend{name: "global-maildir"}
	sdbox := &namedBackend{name: "sdbox"}
	// Mirrors buildMailboxByDriver: known drivers map to a backend, everything
	// else defaults to the global (maildir) backend — never nil.
	byDriver := func(driver string) mailbox.MailboxBackend {
		switch driver {
		case "sdbox", "dbox":
			return sdbox
		case "mdbox":
			return &namedBackend{name: "mdbox"}
		default:
			return global
		}
	}

	cases := []struct {
		name       string
		byDriver   func(string) mailbox.MailboxBackend
		mailLoc    string
		preIndex   string // userInfo.IndexDir already set from a separate userdb field
		wantBox    *namedBackend
		wantDriver string
		wantIndex  string
	}{
		{"empty mailloc → global, no driver", byDriver, "", "", global, "", ""},
		{"unknown driver → global, no stamp", byDriver, "bogus:/srv/x", "", global, "", ""},
		{"no path → parse error → global, no stamp", byDriver, "sdbox", "", global, "", ""},
		{"sdbox selects backend + stamps driver", byDriver, "sdbox:/srv/sd", "", sdbox, "sdbox", ""},
		{"dbox alias selects sdbox backend", byDriver, "dbox:/srv/sd", "", sdbox, "dbox", ""},
		{"factory nil → global but driver stamped", nil, "sdbox:/srv/sd", "", global, "sdbox", ""},
		{"embedded INDEX= modifier fills blank", byDriver, "sdbox:/srv/sd:INDEX=/srv/idx", "", sdbox, "sdbox", "/srv/idx"},
		{"separate index field wins over modifier", byDriver, "sdbox:/srv/sd:INDEX=/srv/idx", "/from/userdb", sdbox, "sdbox", "/from/userdb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui := &mailbox.UserInfo{Username: "u@x.io", Home: "/home/u", IndexDir: c.preIndex}
			box := resolvePersonalStorage(global, c.byDriver, c.mailLoc, ui)
			if box != c.wantBox {
				t.Errorf("box = %v, want %v", box.(*namedBackend).name, c.wantBox.name)
			}
			if ui.Driver != c.wantDriver {
				t.Errorf("driver = %q, want %q", ui.Driver, c.wantDriver)
			}
			if ui.IndexDir != c.wantIndex {
				t.Errorf("indexDir = %q, want %q", ui.IndexDir, c.wantIndex)
			}
		})
	}
}
