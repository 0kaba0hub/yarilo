package main

import (
	"flag"
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
		{"double dash ends flags", []string{"u1", "--", "--not-a-flag"}, false, "personal", []string{"u1", "--not-a-flag"}},
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

func TestParseFlagsUnknownFlagErrors(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(discard{})
	_ = fs.Bool("restore", false, "")
	if err := parseFlags(fs, []string{"u1", "--bogus"}); err == nil {
		t.Error("an unknown flag should still error, not be silently dropped")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
