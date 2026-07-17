package mailbox

import "testing"

type namedBackend struct{ name string }

func (n *namedBackend) OpenUser(*UserInfo) UserMailbox { return nil }

func TestStampLocation(t *testing.T) {
	cases := []struct {
		name       string
		mailLoc    string
		preIndex   string // userdb IndexDir already set (separate field)
		wantErr    bool
		wantDriver string
		wantIndex  string
	}{
		{"empty mailloc → no-op", "", "", false, "", ""},
		{"unknown driver, valid path → error, no stamp", "maildirx:/x", "", true, "", ""},
		{"no colon → error, no stamp", "maildir", "", true, "", ""},
		{"sdbox with path", "sdbox:/srv/sd", "", false, "sdbox", ""},
		{"dbox alias", "dbox:/srv/sd", "", false, "dbox", ""},
		{"mdbox empty path (home-derived) still stamps", "mdbox:", "", false, "mdbox", ""},
		{"embedded INDEX= fills blank", "sdbox:/srv/sd:INDEX=/srv/idx", "", false, "sdbox", "/srv/idx"},
		{"separate index field wins", "sdbox:/srv/sd:INDEX=/srv/idx", "/from/userdb", false, "sdbox", "/from/userdb"},
		{"tilde modifier expands to home", "sdbox:/srv/sd:INDEX=~/idx", "", false, "sdbox", "/home/u/idx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui := &UserInfo{Username: "u@x.io", Home: "/home/u", IndexDir: c.preIndex}
			err := StampLocation(ui, c.mailLoc)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
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

func TestSelectPersonalBackend(t *testing.T) {
	global := &namedBackend{name: "global"}
	sdbox := &namedBackend{name: "sdbox"}
	byDriver := func(driver string) MailboxBackend {
		switch driver {
		case "sdbox", "dbox":
			return sdbox
		default:
			return global // buildMailboxByDriver never returns nil
		}
	}
	cases := []struct {
		name     string
		byDriver func(string) MailboxBackend
		driver   string
		want     *namedBackend
	}{
		{"empty driver → global", byDriver, "", global},
		{"sdbox → per-user backend", byDriver, "sdbox", sdbox},
		{"dbox alias → sdbox backend", byDriver, "dbox", sdbox},
		{"factory nil → global", nil, "sdbox", global},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SelectPersonalBackend(global, c.byDriver, c.driver)
			if got != c.want {
				t.Errorf("got %v, want %v", got.(*namedBackend).name, c.want.name)
			}
		})
	}
	// nil global (IMAP semantics) with no driver returns nil = "use global".
	if got := SelectPersonalBackend(nil, byDriver, ""); got != nil {
		t.Errorf("nil-global empty-driver = %v, want nil", got)
	}
}
