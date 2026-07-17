package mailbox

import "testing"

type namedBackend struct{ name string }

func (n *namedBackend) OpenUser(*UserInfo) UserMailbox { return nil }

func TestResolvePersonalStorage(t *testing.T) {
	global := &namedBackend{name: "global-maildir"}
	sdbox := &namedBackend{name: "sdbox"}
	mdbox := &namedBackend{name: "mdbox"}
	// Mirrors buildMailboxByDriver: known drivers map to a backend, everything
	// else defaults to the global (maildir) backend — never nil.
	byDriver := func(driver string) MailboxBackend {
		switch driver {
		case "sdbox", "dbox":
			return sdbox
		case "mdbox":
			return mdbox
		default:
			return global
		}
	}

	cases := []struct {
		name       string
		byDriver   func(string) MailboxBackend
		mailLoc    string
		preIndex   string // userdb IndexDir already set (separate field)
		wantBox    *namedBackend
		wantDriver string
		wantIndex  string
	}{
		{"empty mailloc → global, no driver", byDriver, "", "", global, "", ""},
		{"unknown driver → global, no stamp", byDriver, "bogus:/srv/x", "", global, "", ""},
		{"no path → parse error → global, no stamp", byDriver, "sdbox", "", global, "", ""},
		{"sdbox selects backend + stamps driver", byDriver, "sdbox:/srv/sd", "", sdbox, "sdbox", ""},
		{"dbox alias selects sdbox backend", byDriver, "dbox:/srv/sd", "", sdbox, "dbox", ""},
		{"mdbox selects backend", byDriver, "mdbox:/srv/md", "", mdbox, "mdbox", ""},
		{"factory nil → global but driver stamped", nil, "sdbox:/srv/sd", "", global, "sdbox", ""},
		{"embedded INDEX= modifier fills blank", byDriver, "sdbox:/srv/sd:INDEX=/srv/idx", "", sdbox, "sdbox", "/srv/idx"},
		{"separate index field wins over modifier", byDriver, "sdbox:/srv/sd:INDEX=/srv/idx", "/from/userdb", sdbox, "sdbox", "/from/userdb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui := &UserInfo{Username: "u@x.io", Home: "/home/u", IndexDir: c.preIndex}
			box := ResolvePersonalStorage(global, c.byDriver, c.mailLoc, ui)
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

// TestStampLocationModifiers checks all four modifiers fill blanks and separate
// fields win, independent of backend selection.
func TestStampLocationModifiers(t *testing.T) {
	ui := &UserInfo{Username: "u@x.io", Home: "/home/u", ControlDir: "/pre/ctrl"}
	StampLocation(ui, "mdbox:/srv/md:INDEX=/i:CONTROL=/c:ALT=/a:VOLATILEDIR=/v")
	if ui.Driver != "mdbox" {
		t.Fatalf("driver = %q", ui.Driver)
	}
	if ui.IndexDir != "/i" || ui.AltDir != "/a" || ui.VolatileDir != "/v" {
		t.Errorf("modifiers not filled: %+v", ui)
	}
	if ui.ControlDir != "/pre/ctrl" { // separate field wins
		t.Errorf("ControlDir overwritten: %q", ui.ControlDir)
	}
}
