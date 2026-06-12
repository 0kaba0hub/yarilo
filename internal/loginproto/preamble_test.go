package loginproto

import (
	"bufio"
	"strings"
	"testing"
)

func TestPreamble_Format(t *testing.T) {
	cases := []struct {
		name string
		p    Preamble
		want string
	}{
		{
			"all fields",
			Preamble{Addr: "1.2.3.4", SessionID: "42", User: "alice", Token: "abc123", Helo: "mail.example.com"},
			"YARILO\tADDR=1.2.3.4\tSESSION=42\tUSER=alice\tTOKEN=abc123\tHELO=mail.example.com\n",
		},
		{
			"no helo",
			Preamble{Addr: "10.0.0.1", SessionID: "1", User: "bob", Token: "tok"},
			"YARILO\tADDR=10.0.0.1\tSESSION=1\tUSER=bob\tTOKEN=tok\n",
		},
		{
			"minimal",
			Preamble{User: "carol", Token: "t"},
			"YARILO\tUSER=carol\tTOKEN=t\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.Format()
			if got != tc.want {
				t.Errorf("Format() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    Preamble
		wantErr bool
	}{
		{
			"all fields",
			"YARILO\tADDR=1.2.3.4\tSESSION=42\tUSER=alice\tTOKEN=abc123\tHELO=mail.example.com",
			Preamble{Addr: "1.2.3.4", SessionID: "42", User: "alice", Token: "abc123", Helo: "mail.example.com"},
			false,
		},
		{
			"no helo",
			"YARILO\tADDR=10.0.0.1\tSESSION=7\tUSER=bob\tTOKEN=xyz",
			Preamble{Addr: "10.0.0.1", SessionID: "7", User: "bob", Token: "xyz"},
			false,
		},
		{
			"unknown keys ignored",
			"YARILO\tUSER=dave\tTOKEN=t\tFOO=bar",
			Preamble{User: "dave", Token: "t"},
			false,
		},
		{
			"not yarilo",
			"IMAP * OK server ready",
			Preamble{},
			true,
		},
		{
			"empty",
			"",
			Preamble{},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ParseLine() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParse_RoundTrip(t *testing.T) {
	original := Preamble{Addr: "192.168.1.1", SessionID: "99", User: "testuser", Token: "deadbeefcafe", Helo: "smtp.example.com"}
	line := original.Format()
	rd := bufio.NewReader(strings.NewReader(line))
	got, err := Parse(rd)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != original {
		t.Errorf("roundtrip: got %+v, want %+v", got, original)
	}
}
