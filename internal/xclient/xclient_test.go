package xclient

import (
	"strings"
	"testing"
)

var decodeXTextCases = []struct {
	name    string
	input   string
	want    string
	wantErr bool
}{
	{"plain", "hello", "hello", false},
	{"unavailable", "[UNAVAILABLE]", "", false},
	{"encoded space", "hello+20world", "hello world", false},
	{"encoded at", "user+40example+2Ecom", "user@example.com", false},
	{"uppercase hex", "A+2BB", "A+B", false},
	{"truncated", "bad+1", "", true},
	{"invalid hex", "bad+ZZ", "", true},
}

func TestDecodeXText(t *testing.T) {
	for _, tc := range decodeXTextCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeXText(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

var encodeXTextCases = []struct {
	name  string
	input string
	want  string
}{
	{"empty → unavailable", "", "[UNAVAILABLE]"},
	{"unreserved chars", "hello-world_123", "hello-world_123"},
	{"space", "hello world", "hello+20world"},
	{"at sign", "user@example.com", "user+40example+2Ecom"},
	{"plus sign", "a+b", "a+2Bb"},
}

func TestEncodeXText(t *testing.T) {
	for _, tc := range encodeXTextCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeXText(tc.input)
			if got != tc.want {
				t.Errorf("EncodeXText(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

var parseCases = []struct {
	name string
	line string
	want Attrs
}{
	{
		"full line with prefix",
		"XCLIENT PROTO=ESMTP ADDR=1+2E2+2E3+2E4 PORT=12345 HELO=mail+2Eexample+2Ecom LOGIN=user SESSION=abc TTL=5",
		Attrs{Proto: "ESMTP", Addr: "1.2.3.4", Port: "12345", Helo: "mail.example.com", Login: "user", Session: "abc", TTL: "5"},
	},
	{
		"partial — only ADDR and PORT",
		"XCLIENT ADDR=10+2E0+2E0+2E1 PORT=25",
		Attrs{Addr: "10.0.0.1", Port: "25"},
	},
	{
		"unknown keys ignored",
		"XCLIENT ADDR=1+2E1+2E1+2E1 UNKNOWN=foo PORT=80",
		Attrs{Addr: "1.1.1.1", Port: "80"},
	},
	{
		"unavailable addr",
		"XCLIENT ADDR=[UNAVAILABLE]",
		Attrs{Addr: ""},
	},
}

func TestParse(t *testing.T) {
	for _, tc := range parseCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.line)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFormat_RoundTrip(t *testing.T) {
	original := Attrs{
		Proto:   "ESMTP",
		Addr:    "192.168.1.1",
		Port:    "54321",
		Helo:    "mail.example.com",
		Login:   "user@example.com",
		Session: "sess-123",
		TTL:     "3",
	}

	lines := Format(original)
	if len(lines) == 0 {
		t.Fatal("Format returned no lines")
	}
	for _, l := range lines {
		if len(l) > maxLineLen {
			t.Errorf("line length %d exceeds max %d: %q", len(l), maxLineLen, l)
		}
	}

	var merged Attrs
	for _, l := range lines {
		a, err := Parse(l)
		if err != nil {
			t.Fatalf("Parse(%q): %v", l, err)
		}
		if a.Proto != "" {
			merged.Proto = a.Proto
		}
		if a.Addr != "" {
			merged.Addr = a.Addr
		}
		if a.Port != "" {
			merged.Port = a.Port
		}
		if a.Helo != "" {
			merged.Helo = a.Helo
		}
		if a.Login != "" {
			merged.Login = a.Login
		}
		if a.Session != "" {
			merged.Session = a.Session
		}
		if a.TTL != "" {
			merged.TTL = a.TTL
		}
	}
	if merged != original {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", merged, original)
	}
}

func TestFormat_LongLineSplit(t *testing.T) {
	// Generate a Login value long enough to force a split.
	a := Attrs{
		Addr:  "1.2.3.4",
		Login: strings.Repeat("x", 480),
	}
	lines := Format(a)
	if len(lines) < 2 {
		t.Errorf("expected split into ≥2 lines for long value, got %d", len(lines))
	}
	for i, l := range lines {
		if len(l) > maxLineLen {
			t.Errorf("line[%d] length %d exceeds %d", i, len(l), maxLineLen)
		}
	}
}
