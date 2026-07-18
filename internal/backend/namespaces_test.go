package backend

import (
	"testing"

	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestBuildNamespaces(t *testing.T) {
	cases := []struct {
		name string
		in   []config.NamespaceConfig
		want []imapsvr.NamespaceSpec
	}{
		{
			name: "empty input returns nil so server applies its built-in default",
			in:   nil,
			want: nil,
		},
		{
			name: "personal + shared + other_users alias",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true},
				{Type: "other_users", Prefix: "user/", Separator: "/", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '/', List: true},
				{Type: imapsvr.NamespaceShared, Prefix: "Shared/", Separator: '/', List: true},
				{Type: imapsvr.NamespaceOther, Prefix: "user/", Separator: '/', List: true},
			},
		},
		{
			name: "per-namespace separator preserved",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: ".", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '.', List: true},
				{Type: imapsvr.NamespaceShared, Prefix: "Shared/", Separator: '/', List: true},
			},
		},
		{
			name: "missing separator defaults to .",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '.', List: true},
			},
		},
		{
			name: "multi-character separator falls back to . with a warning",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "//", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '.', List: true},
			},
		},
		{
			name: "unknown type is skipped",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "bogus", Prefix: "X/", Separator: "/", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			},
		},
		{
			name: "list=false preserved (server still excludes from response)",
			in: []config.NamespaceConfig{
				{Type: "shared", Prefix: "Hidden/", Separator: "/", List: false},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespaceShared, Prefix: "Hidden/", Separator: '/', List: false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildNamespaces(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %d, want %d (got=%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ns[%d]: got %+v want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildNamespaceMailboxes(t *testing.T) {
	cases := []struct {
		name              string
		globalDriver      string
		namespaces        []config.NamespaceConfig
		wantPrefixes      []string // which prefixes should have overrides
		wantTypeForPrefix map[string]any
		wantErr           bool
	}{
		{
			name:         "no namespaces returns nil map",
			globalDriver: "maildir",
			namespaces:   nil,
			wantPrefixes: nil,
		},
		{
			name:         "all match global driver — no overrides built",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "maildir:/var/yarilo/shared"},
			},
			wantPrefixes: nil,
		},
		{
			name:         "shared mdbox overrides global maildir",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "mdbox:/var/yarilo/shared"},
			},
			wantPrefixes: []string{"Shared/"},
			wantTypeForPrefix: map[string]any{
				"Shared/": (*mdbox.Backend)(nil),
			},
		},
		{
			name:         "two namespaces same non-default driver share one backend instance",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "mdbox:/var/yarilo/shared"},
				{Type: "shared", Prefix: "Public/", Separator: "/", List: true, Location: "mdbox:/var/yarilo/public"},
			},
			wantPrefixes: []string{"Shared/", "Public/"},
		},
		{
			name:         "personal global mdbox, shared maildir override",
			globalDriver: "mdbox",
			namespaces: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "/", List: true},
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "maildir:/var/yarilo/shared"},
			},
			wantPrefixes: []string{"Shared/"},
			wantTypeForPrefix: map[string]any{
				"Shared/": (*maildir.Backend)(nil),
			},
		},
		{
			name:         "dbox override",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "dbox:/var/yarilo/shared"},
			},
			wantPrefixes: []string{"Shared/"},
			wantTypeForPrefix: map[string]any{
				"Shared/": (*dboxv2.Backend)(nil),
			},
		},
		{
			name:         "ns without location is skipped",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "other", Prefix: "user/", Separator: "/", List: true}, // no location:
			},
			wantPrefixes: nil,
		},
		{
			name:         "missing colon in location errors",
			globalDriver: "maildir",
			namespaces: []config.NamespaceConfig{
				{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "maildir-no-colon"},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildNamespaceMailboxes(tc.namespaces, tc.globalDriver, config.StorageConfig{}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tc.wantPrefixes) {
				t.Fatalf("len(overrides) = %d, want %d (got=%v)", len(got), len(tc.wantPrefixes), keys(got))
			}
			for _, p := range tc.wantPrefixes {
				if _, ok := got[p]; !ok {
					t.Errorf("missing override for prefix %q (have %v)", p, keys(got))
				}
			}
			for p, want := range tc.wantTypeForPrefix {
				gotV, ok := got[p]
				if !ok {
					t.Errorf("override %q missing for type check", p)
					continue
				}
				switch want.(type) {
				case *maildir.Backend:
					if _, ok := gotV.(*maildir.Backend); !ok {
						t.Errorf("override %q: got %T, want *maildir.Backend", p, gotV)
					}
				case *mdbox.Backend:
					if _, ok := gotV.(*mdbox.Backend); !ok {
						t.Errorf("override %q: got %T, want *mdbox.Backend", p, gotV)
					}
				case *dboxv2.Backend:
					if _, ok := gotV.(*dboxv2.Backend); !ok {
						t.Errorf("override %q: got %T, want *dboxv2.Backend", p, gotV)
					}
				}
			}
		})
	}
}

func TestBuildNamespaceMailboxesSharesPerDriverInstance(t *testing.T) {
	// Two namespaces declaring the same non-default driver must point
	// at the SAME *Backend instance to keep per-process bookkeeping
	// (hostname/pid/counter) consistent and avoid duplicate locker
	// registrations.
	got, err := buildNamespaceMailboxes([]config.NamespaceConfig{
		{Type: "shared", Prefix: "Shared/", Separator: "/", List: true, Location: "mdbox:/var/a"},
		{Type: "shared", Prefix: "Public/", Separator: "/", List: true, Location: "mdbox:/var/b"},
	}, "maildir", config.StorageConfig{}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got["Shared/"] != got["Public/"] {
		t.Errorf("same-driver overrides must share Backend instance: shared=%p public=%p", got["Shared/"], got["Public/"])
	}
}

func keys(m map[string]mailbox.MailboxBackend) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
