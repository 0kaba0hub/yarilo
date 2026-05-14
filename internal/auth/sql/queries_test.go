package sql

import (
	"reflect"
	"testing"
)

func TestSubstituteVars(t *testing.T) {
	cases := []struct {
		name      string
		driver    string
		query     string
		username  string
		wantQuery string
		wantArgs  []any
	}{
		{
			name:      "no vars sqlite",
			driver:    "sqlite",
			query:     "SELECT 1",
			username:  "x@y",
			wantQuery: "SELECT 1",
			wantArgs:  nil,
		},
		{
			name:      "%u sqlite",
			driver:    "sqlite",
			query:     "SELECT password FROM users WHERE email = '%u'",
			username:  "alice@example.com",
			wantQuery: "SELECT password FROM users WHERE email = '?'",
			wantArgs:  []any{"alice@example.com"},
		},
		{
			name:      "%u mysql",
			driver:    "mysql",
			query:     "SELECT password FROM users WHERE email = '%u'",
			username:  "alice@example.com",
			wantQuery: "SELECT password FROM users WHERE email = '?'",
			wantArgs:  []any{"alice@example.com"},
		},
		{
			name:      "%u postgres",
			driver:    "postgres",
			query:     "SELECT password FROM users WHERE email = '%u'",
			username:  "alice@example.com",
			wantQuery: "SELECT password FROM users WHERE email = '$1'",
			wantArgs:  []any{"alice@example.com"},
		},
		{
			name:      "all vars postgres",
			driver:    "postgres",
			query:     "SELECT * FROM users WHERE name = '%n' AND domain = '%d' AND full = '%u'",
			username:  "alice@example.com",
			wantQuery: "SELECT * FROM users WHERE name = '$1' AND domain = '$2' AND full = '$3'",
			wantArgs:  []any{"alice", "example.com", "alice@example.com"},
		},
		{
			name:      "unknown %x left as-is",
			driver:    "sqlite",
			query:     "SELECT %u, %x FROM t",
			username:  "alice",
			wantQuery: "SELECT ?, %x FROM t",
			wantArgs:  []any{"alice"},
		},
		{
			name:      "username without domain",
			driver:    "mysql",
			query:     "SELECT %n / %d",
			username:  "alice",
			wantQuery: "SELECT ? / ?",
			wantArgs:  []any{"alice", ""},
		},
		{
			name:      "trailing percent ignored",
			driver:    "sqlite",
			query:     "value LIKE '%'",
			username:  "x",
			wantQuery: "value LIKE '%'",
			wantArgs:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotQ, gotA := substituteVars(tc.driver, tc.query, tc.username)
			if gotQ != tc.wantQuery {
				t.Errorf("query:\n got:  %q\n want: %q", gotQ, tc.wantQuery)
			}
			if !reflect.DeepEqual(gotA, tc.wantArgs) {
				t.Errorf("args:\n got:  %#v\n want: %#v", gotA, tc.wantArgs)
			}
		})
	}
}

func TestSplitUser(t *testing.T) {
	cases := []struct {
		in, local, domain string
	}{
		{"alice@example.com", "alice", "example.com"},
		{"alice", "alice", ""},
		{"@example.com", "", "example.com"},
		{"alice@sub.example.com", "alice", "sub.example.com"},
		{"", "", ""},
	}
	for _, tc := range cases {
		l, d := splitUser(tc.in)
		if l != tc.local || d != tc.domain {
			t.Errorf("splitUser(%q) = (%q, %q), want (%q, %q)", tc.in, l, d, tc.local, tc.domain)
		}
	}
}
