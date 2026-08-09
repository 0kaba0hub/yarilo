package mailbox

import "testing"

// The bucket a user lands in has to be the one the reference would pick, or a
// migrated config quietly redistributes every user and the layout it describes
// is not the layout it produces. These are computed from the reference's own
// rule: the digest read as a big-endian integer from its last eight bytes,
// modulo, then hex-padded.
func TestHashBucketMatchesTheReference(t *testing.T) {
	tests := []struct {
		user string
		want string
	}{
		{"u1@d00001.test", "d5"},
		{"bob@example.org", "61"},
		{"u51@d00002.test", "35"},
	}
	for _, tc := range tests {
		got, err := ExpandTemplate("%{user | sha1 % 256 | hex(2)}", TemplateVars{User: tc.user})
		if err != nil {
			t.Fatalf("%s: %v", tc.user, err)
		}
		if got != tc.want {
			t.Errorf("%s bucketed to %q, want %q", tc.user, got, tc.want)
		}
	}
}

// Reading the digest from the wrong end is the mistake that produces a
// plausible-looking bucket for every user and the right one for none, so the
// two ends must not agree on the input the test uses.
func TestHashBucketDependsOnTheDigestTail(t *testing.T) {
	got, err := ExpandTemplate("%{user | sha1 % 256 | hex(2)}", TemplateVars{User: "u1@d00001.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got == "e4" {
		t.Error("the bucket came from the head of the digest, not its tail")
	}
}

func TestExpandTemplateBothDialects(t *testing.T) {
	vars := TemplateVars{User: "Alice@Example.COM", Home: "/home/alice"}
	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{"full user", "%{user}", "Alice@Example.COM"},
		{"local part", "%{user | username}", "Alice"},
		{"domain", "%{user | domain}", "Example.COM"},
		{"lowered", "%{user | lower}", "alice@example.com"},
		{"chained", "%{user | domain | lower}", "example.com"},
		{"home", "%{home}/Maildir", "/home/alice/Maildir"},
		{"legacy user", "%u", "Alice@Example.COM"},
		{"legacy local", "%n", "Alice"},
		{"legacy domain", "%d", "Example.COM"},
		{"legacy home", "%h", "/home/alice"},
		{"literal percent", "100%%", "100%"},
		{"mixed dialects", "%{user | domain}/%n", "Example.COM/Alice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandTemplate(tc.tmpl, vars)
			if err != nil {
				t.Fatalf("%s: %v", tc.tmpl, err)
			}
			if got != tc.want {
				t.Errorf("%s expanded to %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

// hex pads and truncates from the same end the reference does: a positive width
// keeps the low digits, a negative one keeps the high ones.
func TestHexWidth(t *testing.T) {
	tests := []struct {
		tmpl string
		want string
	}{
		{"%{user | sha1 % 65536 | hex(2)}", "d5"},
		{"%{user | sha1 % 65536 | hex(4)}", "8fd5"},
		{"%{user | sha1 % 65536 | hex(-2)}", "8f"},
		{"%{user | sha1 % 65536 | hex(8)}", "00008fd5"},
	}
	for _, tc := range tests {
		got, err := ExpandTemplate(tc.tmpl, TemplateVars{User: "u1@d00001.test"})
		if err != nil {
			t.Fatalf("%s: %v", tc.tmpl, err)
		}
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

// A template we cannot expand is a misconfiguration. Passing it through
// verbatim is what let a 2.4 config produce a directory literally named
// "%{user | sha1 % 256 | hex(2)}" — created, used, and silent.
func TestUnexpandableTemplatesAreRefused(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
	}{
		{"unknown variable", "%{nope}"},
		{"unknown filter", "%{user | nope}"},
		{"unknown short variable", "/var/%z/mail"},
		{"unclosed expression", "/var/%{user/mail"},
		{"trailing percent", "/var/mail%"},
		{"zero modulo", "%{user | sha1 % 0 | hex(2)}"},
		{"hash without a modulo", "%{user | sha1 | hex(2)}"},
		{"hash left as bytes", "%{user | sha1}"},
		{"text where a number is needed", "%{user | hex(2)}"},
		{"malformed hash variable", "%2.Nu"},
		{"filter on a number", "%{user | sha1 % 256 | domain}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ExpandTemplate(tc.tmpl, TemplateVars{User: "u@example.org", Home: "/h"}); err == nil {
				t.Errorf("%s expanded to %q instead of being refused", tc.tmpl, got)
			}
		})
	}
}

func TestValidateTemplateAcceptsWhatWeShip(t *testing.T) {
	shipped := []string{
		"/tmp/yarilo-volatile/%2.256Nu/%u",
		"/tmp/yarilo-volatile/%{user | sha1 % 256 | hex(2)}/%{user}",
		"%d/%u",
		"/var/mail/vhosts",
		"~/index",
		"",
	}
	for _, tmpl := range shipped {
		if err := ValidateTemplate(tmpl); err != nil {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
}

// "~" is a third spelling of the home directory, alongside %h and %{home}. All
// three have to produce the same path, or a config that uses one convention
// silently builds a different tree than one that uses another.
func TestTildeIsTheHomeVariable(t *testing.T) {
	vars := TemplateVars{User: "alice@example.com", Home: "/srv/mail/alice"}
	tests := []struct {
		tmpl string
		want string
	}{
		{"~", "/srv/mail/alice"},
		{"~/index", "/srv/mail/alice/index"},
		{"%h/index", "/srv/mail/alice/index"},
		{"%{home}/index", "/srv/mail/alice/index"},
		{"~/index/%{user | domain}", "/srv/mail/alice/index/example.com"},
		{"/var/~/index", "/var/~/index"}, // only a leading ~ is the home
	}
	for _, tc := range tests {
		got, err := ExpandTemplate(tc.tmpl, vars)
		if err != nil {
			t.Fatalf("%s: %v", tc.tmpl, err)
		}
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}
