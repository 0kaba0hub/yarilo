package mailbox

import "testing"

func TestExpandVars(t *testing.T) {
	cases := []struct {
		name           string
		template, user string
		want           string
	}{
		{"empty template", "", "alice@example.com", ""},
		{"no vars", "fixed/path", "alice@example.com", "fixed/path"},
		{"%d/%n", "%d/%n", "alice@example.com", "example.com/alice"},
		{"%u", "%u", "alice@example.com", "alice@example.com"},
		{"%n only", "%n", "alice@example.com", "alice"},
		{"%d only", "%d", "alice@example.com", "example.com"},
		{"username without @", "%d/%n", "root", "/root"},
		{"literal %%", "100%%-uptime", "alice@x", "100%-uptime"},
		{"unknown %x preserved", "%x/%n", "alice@x", "%x/alice"},
		{"trailing percent preserved", "path-%", "alice@x", "path-%"},
		{"mixed", "/srv/%d/%n/%u", "alice@example.com", "/srv/example.com/alice/alice@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandVars(tc.template, tc.user)
			if got != tc.want {
				t.Errorf("ExpandVars(%q, %q) = %q, want %q", tc.template, tc.user, got, tc.want)
			}
		})
	}
}

func TestResolver_Resolve(t *testing.T) {
	r := &Resolver{Root: "/var/mail/vhosts", HomeTemplate: "%d/%n"}
	cases := []struct {
		name               string
		username, override string
		want               string
	}{
		{"no override → template", "alice@example.com", "", "/var/mail/vhosts/example.com/alice"},
		{"abs override wins", "alice@example.com", "/srv/heavy/alice", "/srv/heavy/alice"},
		{"relative override joined to root", "alice@example.com", "ssd/alice", "/var/mail/vhosts/ssd/alice"},
		{"empty default template", "u@x", "", "/var/mail/vhosts/x/u"},
		{"empty username, no override", "", "", "/var/mail/vhosts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Resolve(tc.username, tc.override)
			if got != tc.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", tc.username, tc.override, got, tc.want)
			}
		})
	}
}

func TestResolver_DefaultTemplate(t *testing.T) {
	// HomeTemplate empty must default to "%d/%u" (full login address).
	r := &Resolver{Root: "/root"}
	got := r.Resolve("alice@example.com", "")
	if got != "/root/example.com/alice@example.com" {
		t.Errorf("default template: got %q, want /root/example.com/alice@example.com", got)
	}
}

func TestResolver_UserInfo(t *testing.T) {
	r := &Resolver{Root: "/var/mail", HomeTemplate: "%d/%n"}
	ui := r.UserInfo("bob@example.com", "")
	if ui.Username != "bob@example.com" {
		t.Errorf("Username: got %q", ui.Username)
	}
	if ui.Home != "/var/mail/example.com/bob" {
		t.Errorf("Home: got %q", ui.Home)
	}
}

func TestParseLocation(t *testing.T) {
	cases := []struct {
		name    string
		loc     string
		ui      *UserInfo
		want    Location
		wantOK  bool
		wantErr bool
	}{
		{"empty returns ok=false", "", nil, Location{}, false, false},
		{"maildir literal", "maildir:/var/yarilo/shared", nil, Location{Driver: "maildir", Path: "/var/yarilo/shared"}, true, false},
		{"maildir templated home", "maildir:%h", &UserInfo{Home: "/srv/u/alice"}, Location{Driver: "maildir", Path: "/srv/u/alice"}, true, false},
		{"maildir templated %d/%n", "maildir:/srv/%d/%n", &UserInfo{Username: "alice@x.io"}, Location{Driver: "maildir", Path: "/srv/x.io/alice"}, true, false},
		{"dbox accepted (forward-compat)", "dbox:/var/dbox", nil, Location{Driver: "dbox", Path: "/var/dbox"}, true, false},
		{"mdbox accepted (forward-compat)", "mdbox:/var/mdbox", nil, Location{Driver: "mdbox", Path: "/var/mdbox"}, true, false},
		{"missing colon errors", "maildir", nil, Location{}, false, true},
		{"unknown driver errors", "weird:/foo", nil, Location{}, false, true},
		{"empty path after expansion errors", "maildir:%h", nil, Location{}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := ParseLocation(tc.loc, tc.ui)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("Location = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseMailLocationMods(t *testing.T) {
	cases := []struct {
		name string
		loc  string
		want map[string]string
	}{
		{"no modifiers", "maildir:~/Maildir", nil},
		{"driver only", "maildir", nil},
		{"VOLATILEDIR", "maildir:~/Maildir:VOLATILEDIR=/tmp/v/%u", map[string]string{"VOLATILEDIR": "/tmp/v/%u"}},
		{"multiple mods", "maildir:~/Maildir:VOLATILEDIR=/tmp/v:INDEX=/tmp/idx", map[string]string{"VOLATILEDIR": "/tmp/v", "INDEX": "/tmp/idx"}},
		{"lowercase key normalised", "maildir:~/Maildir:volatiledir=/tmp/v", map[string]string{"VOLATILEDIR": "/tmp/v"}},
		{"mod without value skipped", "maildir:~/Maildir:NOVALUE", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMailLocationMods(tc.loc)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			for k, wantV := range tc.want {
				if gotV, ok := got[k]; !ok || gotV != wantV {
					t.Errorf("mod[%q]: got %q (ok=%v), want %q", k, gotV, ok, wantV)
				}
			}
		})
	}
}

func TestResolverDefaultVolatileDir(t *testing.T) {
	r := &Resolver{
		Root:               "/mail",
		HomeTemplate:       "%d/%n",
		DefaultVolatileDir: "/run/volatile/%d/%n",
	}
	ui := r.UserInfo("alice@example.com", "")
	want := "/run/volatile/example.com/alice"
	if ui.VolatileDir != want {
		t.Errorf("VolatileDir = %q, want %q", ui.VolatileDir, want)
	}
}

func TestResolverDefaultVolatileDirWithHome(t *testing.T) {
	r := &Resolver{
		Root:               "/mail",
		HomeTemplate:       "%d/%n",
		DefaultVolatileDir: "/run/volatile/%h",
	}
	ui := r.UserInfo("bob@test.com", "")
	want := "/run/volatile/" + ui.Home
	if ui.VolatileDir != want {
		t.Errorf("VolatileDir = %q, want %q", ui.VolatileDir, want)
	}
}

func TestResolverDefaultIndexDir(t *testing.T) {
	r := &Resolver{
		Root:            "/mail",
		HomeTemplate:    "%d/%n",
		DefaultIndexDir: "/var/index/%d/%n",
	}
	ui := r.UserInfo("alice@example.com", "")
	want := "/var/index/example.com/alice"
	if ui.IndexDir != want {
		t.Errorf("IndexDir = %q, want %q", ui.IndexDir, want)
	}
}

func TestResolverDefaultIndexDirWithHome(t *testing.T) {
	r := &Resolver{
		Root:            "/mail",
		HomeTemplate:    "%d/%n",
		DefaultIndexDir: "/var/index/%h",
	}
	ui := r.UserInfo("bob@test.com", "")
	want := "/var/index/" + ui.Home
	if ui.IndexDir != want {
		t.Errorf("IndexDir = %q, want %q", ui.IndexDir, want)
	}
}

func TestParseMailLocationModsIndex(t *testing.T) {
	mods := ParseMailLocationMods("maildir:~/Maildir:INDEX=/srv/idx/%u:VOLATILEDIR=/tmp/v")
	if mods["INDEX"] != "/srv/idx/%u" {
		t.Errorf("INDEX = %q, want /srv/idx/%%u", mods["INDEX"])
	}
	if mods["VOLATILEDIR"] != "/tmp/v" {
		t.Errorf("VOLATILEDIR = %q, want /tmp/v", mods["VOLATILEDIR"])
	}
}

func TestResolverDefaultControlDir(t *testing.T) {
	r := &Resolver{
		Root:              "/mail",
		HomeTemplate:      "%d/%n",
		DefaultControlDir: "/var/control/%d/%n",
	}
	ui := r.UserInfo("alice@example.com", "")
	want := "/var/control/example.com/alice"
	if ui.ControlDir != want {
		t.Errorf("ControlDir = %q, want %q", ui.ControlDir, want)
	}
}

func TestResolverDefaultControlDirWithHome(t *testing.T) {
	r := &Resolver{
		Root:              "/mail",
		HomeTemplate:      "%d/%n",
		DefaultControlDir: "/var/control/%h",
	}
	ui := r.UserInfo("bob@test.com", "")
	want := "/var/control/" + ui.Home
	if ui.ControlDir != want {
		t.Errorf("ControlDir = %q, want %q", ui.ControlDir, want)
	}
}

func TestParseMailLocationModsControl(t *testing.T) {
	mods := ParseMailLocationMods("maildir:~/Maildir:CONTROL=/srv/ctrl/%u:INDEX=/srv/idx")
	if mods["CONTROL"] != "/srv/ctrl/%u" {
		t.Errorf("CONTROL = %q, want /srv/ctrl/%%u", mods["CONTROL"])
	}
	if mods["INDEX"] != "/srv/idx" {
		t.Errorf("INDEX = %q, want /srv/idx", mods["INDEX"])
	}
}

func TestResolverDefaultAltDir(t *testing.T) {
	r := &Resolver{
		Root:          "/mail",
		HomeTemplate:  "%d/%n",
		DefaultAltDir: "/mnt/cold/%d/%n",
	}
	ui := r.UserInfo("alice@example.com", "")
	want := "/mnt/cold/example.com/alice"
	if ui.AltDir != want {
		t.Errorf("AltDir = %q, want %q", ui.AltDir, want)
	}
}

func TestResolverDefaultAltDirWithHome(t *testing.T) {
	r := &Resolver{
		Root:          "/mail",
		HomeTemplate:  "%d/%n",
		DefaultAltDir: "/mnt/cold/%h",
	}
	ui := r.UserInfo("bob@test.com", "")
	want := "/mnt/cold/" + ui.Home
	if ui.AltDir != want {
		t.Errorf("AltDir = %q, want %q", ui.AltDir, want)
	}
}

func TestParseMailLocationModsAlt(t *testing.T) {
	mods := ParseMailLocationMods("maildir:~/Maildir:ALT=/mnt/cold/%u:INDEX=/srv/idx")
	if mods["ALT"] != "/mnt/cold/%u" {
		t.Errorf("ALT = %q, want /mnt/cold/%%u", mods["ALT"])
	}
	if mods["INDEX"] != "/srv/idx" {
		t.Errorf("INDEX = %q, want /srv/idx", mods["INDEX"])
	}
}
