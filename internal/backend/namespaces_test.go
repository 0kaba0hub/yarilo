package backend

import (
	"testing"

	imapsvr "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/pkg/config"
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
			name: "missing separator defaults to /",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '/', List: true},
			},
		},
		{
			name: "multi-character separator falls back to / with a warning",
			in: []config.NamespaceConfig{
				{Type: "personal", Prefix: "", Separator: "//", List: true},
			},
			want: []imapsvr.NamespaceSpec{
				{Type: imapsvr.NamespacePersonal, Prefix: "", Separator: '/', List: true},
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
