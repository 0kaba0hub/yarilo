package main

import (
	"flag"
	"testing"
)

func TestParseUserArg(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantUser   string
		wantFolder string
		wantErr    bool
	}{
		{name: "user then flags", args: []string{"u1@x", "--folder", "Sent"}, wantUser: "u1@x", wantFolder: "Sent"},
		{name: "flags then user", args: []string{"--folder", "Sent", "u1@x"}, wantUser: "u1@x", wantFolder: "Sent"},
		{name: "user only", args: []string{"u1@x"}, wantUser: "u1@x", wantFolder: "INBOX"},
		{name: "no args", args: nil, wantUser: ""},
		{name: "two positionals", args: []string{"u1@x", "u2@x"}, wantUser: ""},
		{name: "unknown flag", args: []string{"u1@x", "--bogus"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			folder := fs.String("folder", "INBOX", "")
			user, err := parseUserArg(fs, tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if user != tc.wantUser {
				t.Fatalf("user = %q, want %q", user, tc.wantUser)
			}
			if tc.wantUser != "" && *folder != tc.wantFolder {
				t.Fatalf("folder = %q, want %q", *folder, tc.wantFolder)
			}
		})
	}
}
