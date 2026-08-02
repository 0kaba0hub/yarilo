package mailbox

import "testing"

// A location value means the same path whether it reaches the resolver from
// the config or from the userdb, so "~/" has to resolve in both.
func TestExpandLocation(t *testing.T) {
	const home = "/var/mail/vhosts/d1.test/u1@d1.test"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "tilde", in: "~/index", want: home + "/index"},
		{name: "tilde alone is not a prefix", in: "~index", want: "~index"},
		{name: "home var", in: "%h/index", want: home + "/index"},
		{name: "absolute untouched", in: "/srv/index", want: "/srv/index"},
		{name: "user var", in: "/srv/%u/index", want: "/srv/u1@d1.test/index"},
		{name: "local part and domain", in: "/srv/%d/%n", want: "/srv/d1.test/u1"},
		{name: "tilde with vars", in: "~/idx/%n", want: home + "/idx/u1"},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandLocation(tt.in, home, "u1@d1.test"); got != tt.want {
				t.Errorf("ExpandLocation(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Every location key resolves the same way. INDEX=, CONTROL=, ALT= and
// VOLATILEDIR= used to skip the "~/" step that mail_path already had.
func TestResolverExpandsTildeForEveryLocation(t *testing.T) {
	r := &Resolver{
		Root:               "/var/mail/vhosts",
		HomeTemplate:       "%d/%u",
		DefaultIndexDir:    "~/index",
		DefaultControlDir:  "~/control",
		DefaultAltDir:      "~/alt",
		DefaultVolatileDir: "~/volatile",
		DefaultMailPath:    "~/maildir",
	}
	ui := r.UserInfo("u1@d1.test", "")
	home := "/var/mail/vhosts/d1.test/u1@d1.test"
	for _, tc := range []struct {
		key, got, want string
	}{
		{"INDEX", ui.IndexDir, home + "/index"},
		{"CONTROL", ui.ControlDir, home + "/control"},
		{"ALT", ui.AltDir, home + "/alt"},
		{"VOLATILEDIR", ui.VolatileDir, home + "/volatile"},
		{"mail_path", ui.MailPath, home + "/maildir"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, tc.got, tc.want)
		}
	}
}

// The forms that already worked keep working: %h, absolute paths and the
// per-user variables are unchanged by the tilde handling.
func TestResolverKeepsExistingLocationForms(t *testing.T) {
	r := &Resolver{
		Root:               "/var/mail/vhosts",
		HomeTemplate:       "%d/%n",
		DefaultIndexDir:    "%h/index",
		DefaultControlDir:  "/srv/control/%u",
		DefaultAltDir:      "/srv/alt/%d/%n",
		DefaultVolatileDir: "/srv/volatile",
		DefaultMailPath:    "%h/maildir",
	}
	ui := r.UserInfo("u1@d1.test", "")
	home := "/var/mail/vhosts/d1.test/u1"
	for _, tc := range []struct {
		key, got, want string
	}{
		{"INDEX", ui.IndexDir, home + "/index"},
		{"CONTROL", ui.ControlDir, "/srv/control/u1@d1.test"},
		{"ALT", ui.AltDir, "/srv/alt/d1.test/u1"},
		{"VOLATILEDIR", ui.VolatileDir, "/srv/volatile"},
		{"mail_path", ui.MailPath, home + "/maildir"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, tc.got, tc.want)
		}
	}
}

// An unset template leaves its field empty rather than resolving to the home.
func TestResolverLeavesUnsetLocationsEmpty(t *testing.T) {
	r := &Resolver{Root: "/var/mail/vhosts", HomeTemplate: "%d/%u"}
	ui := r.UserInfo("u1@d1.test", "")
	for _, tc := range []struct{ key, got string }{
		{"INDEX", ui.IndexDir},
		{"CONTROL", ui.ControlDir},
		{"ALT", ui.AltDir},
		{"VOLATILEDIR", ui.VolatileDir},
		{"mail_path", ui.MailPath},
	} {
		if tc.got != "" {
			t.Errorf("%s = %q, want empty", tc.key, tc.got)
		}
	}
}
