package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseFlagsPositionIndependent(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantBool bool
		wantStr  string
		wantPos  []string
	}{
		{"flags before positional", []string{"--restore", "--ns", "shared", "u1"}, true, "shared", []string{"u1"}},
		{"bool flag after positional", []string{"u1", "--restore"}, true, "personal", []string{"u1"}},
		{"value flag after positional", []string{"u1", "--ns", "shared"}, false, "shared", []string{"u1"}},
		{"interspersed", []string{"--restore", "u1", "--ns", "shared"}, true, "shared", []string{"u1"}},
		{"equals form after positional", []string{"u1", "--ns=shared", "--restore"}, true, "shared", []string{"u1"}},
		{"no flags", []string{"u1"}, false, "personal", []string{"u1"}},
		{"double dash protects dash-arg", []string{"--", "-weird"}, false, "personal", []string{"-weird"}},
		{"two positionals with trailing flag", []string{"a", "b", "--restore"}, true, "personal", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			restore := fs.Bool("restore", false, "")
			ns := fs.String("ns", "personal", "")
			if err := parseFlags(fs, c.args); err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if *restore != c.wantBool {
				t.Errorf("restore = %v, want %v", *restore, c.wantBool)
			}
			if *ns != c.wantStr {
				t.Errorf("ns = %q, want %q", *ns, c.wantStr)
			}
			got := fs.Args()
			if len(got) != len(c.wantPos) {
				t.Fatalf("positionals = %v, want %v", got, c.wantPos)
			}
			for i := range got {
				if got[i] != c.wantPos[i] {
					t.Errorf("positional[%d] = %q, want %q", i, got[i], c.wantPos[i])
				}
			}
		})
	}
}

func TestParseFlagsUnknownFlagBeforePositionalErrors(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(discard{})
	_ = fs.Bool("restore", false, "")
	// A dash-token before any positional that names no registered flag is an
	// unknown flag — stdlib errors, and so must we (not silently drop it).
	if err := parseFlags(fs, []string{"--bogus", "u1"}); err == nil {
		t.Error("an unknown flag before the positional should error")
	}
}

// TestParseFlagsDashPositionals is the regression guard for #617 review: a
// dash-prefixed token that is not a registered flag must stay a positional,
// exactly as stdlib flag.Parse treats it once scanning has hit a positional.
func TestParseFlagsDashPositionals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		pos  []string
	}{
		// dict atomic-inc <dict> <key> <delta> — negative delta is a documented use.
		{"negative delta", []string{"mydict", "counter", "-5"}, []string{"mydict", "counter", "-5"}},
		// acl set <user> <mailbox> <id> <rights> — RFC 4314 revoke uses a leading '-'.
		{"acl revoke rights", []string{"alice", "INBOX", "admin", "-lrs"}, []string{"alice", "INBOX", "admin", "-lrs"}},
		// a registered flag still reorders even alongside a dash-positional.
		{"flag plus dash-positional", []string{"counter", "-5", "--restore"}, []string{"counter", "-5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(discard{})
			restore := fs.Bool("restore", false, "")
			if err := parseFlags(fs, c.args); err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			got := fs.Args()
			if len(got) != len(c.pos) {
				t.Fatalf("positionals = %v, want %v", got, c.pos)
			}
			for i := range got {
				if got[i] != c.pos[i] {
					t.Errorf("positional[%d] = %q, want %q", i, got[i], c.pos[i])
				}
			}
			if c.name == "flag plus dash-positional" && !*restore {
				t.Error("--restore after a dash-positional should still be applied")
			}
		})
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestExtractGlobalFlags_PositionIndependent covers #836: -O/--url and other
// global flags must be pulled from ANY position (before the plane, between
// plane and command, trailing), while subcommand flags/positionals stay in
// rest untouched.
func TestExtractGlobalFlags_PositionIndependent(t *testing.T) {
	registerGlobalFlags() // populate flag.CommandLine so the scan can recognize globals

	cases := []struct {
		name    string
		argv    []string
		globals []string
		rest    []string
	}{
		{
			"A prefix",
			[]string{"-O", "json", "director", "ring", "status"},
			[]string{"-O", "json"},
			[]string{"director", "ring", "status"},
		},
		{
			"B trailing (the silent-swallow bug)",
			[]string{"director", "ring", "status", "-O", "json"},
			[]string{"-O", "json"},
			[]string{"director", "ring", "status"},
		},
		{
			"C between plane and command",
			[]string{"director", "-O", "json", "ring", "status"},
			[]string{"-O", "json"},
			[]string{"director", "ring", "status"},
		},
		{
			"D backends trailing",
			[]string{"director", "backends", "list", "-O", "json"},
			[]string{"-O", "json"},
			[]string{"director", "backends", "list"},
		},
		{
			"equals form trailing",
			[]string{"director", "ring", "status", "-O=json"},
			[]string{"-O=json"},
			[]string{"director", "ring", "status"},
		},
		{
			"multiple globals mixed positions",
			[]string{"--url", "http://x:9103", "director", "ring", "status", "-O", "json"},
			[]string{"--url", "http://x:9103", "-O", "json"},
			[]string{"director", "ring", "status"},
		},
		{
			"subcommand flag stays in rest",
			[]string{"director", "backends", "add", "10.0.0.3", "--port", "993", "-O", "json"},
			[]string{"-O", "json"},
			[]string{"director", "backends", "add", "10.0.0.3", "--port", "993"},
		},
		{
			"-- terminator passes through to rest",
			[]string{"director", "ring", "status", "--", "-O", "json"},
			nil,
			[]string{"director", "ring", "status", "--", "-O", "json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globals, rest := extractGlobalFlags(tc.argv)
			if !slices.Equal(globals, tc.globals) {
				t.Errorf("globals = %v, want %v", globals, tc.globals)
			}
			if !slices.Equal(rest, tc.rest) {
				t.Errorf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
}
