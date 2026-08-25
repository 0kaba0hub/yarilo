package submission

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestParseWorkarounds(t *testing.T) {
	cases := []struct {
		input []string
		want  submissionWorkarounds
	}{
		{nil, 0},
		{[]string{"whitespace-before-path"}, workaroundWhitespaceBeforePath},
		{[]string{"mailbox-for-path"}, workaroundMailboxForPath},
		{[]string{"whitespace-before-path", "mailbox-for-path"}, workaroundWhitespaceBeforePath | workaroundMailboxForPath},
		{[]string{"WHITESPACE-BEFORE-PATH"}, workaroundWhitespaceBeforePath},
		{[]string{"unknown-thing"}, 0},
	}
	for _, tc := range cases {
		got, _ := parseWorkarounds(tc.input)
		if got != tc.want {
			t.Errorf("parseWorkarounds(%v) = %b, want %b", tc.input, got, tc.want)
		}
	}
}

func TestApplyWorkarounds_WhitespaceBeforePath(t *testing.T) {
	c := &workaroundConn{workarounds: workaroundWhitespaceBeforePath}
	cases := []struct{ in, want string }{
		{"MAIL FROM: <alice@example.com>\r\n", "MAIL FROM:<alice@example.com>\r\n"},
		{"RCPT TO:  <bob@example.com>\r\n", "RCPT TO:<bob@example.com>\r\n"},
		{"MAIL FROM:<alice@example.com>\r\n", "MAIL FROM:<alice@example.com>\r\n"},
		{"DATA\r\n", "DATA\r\n"},
	}
	for _, tc := range cases {
		got := c.applyWorkarounds(tc.in)
		if got != tc.want {
			t.Errorf("applyWorkarounds(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyWorkarounds_MailboxForPath(t *testing.T) {
	c := &workaroundConn{workarounds: workaroundMailboxForPath}
	cases := []struct{ in, want string }{
		{"MAIL FROM:alice@example.com\r\n", "MAIL FROM:<alice@example.com>\r\n"},
		{"RCPT TO:bob@example.com\r\n", "RCPT TO:<bob@example.com>\r\n"},
		{"MAIL FROM:<alice@example.com>\r\n", "MAIL FROM:<alice@example.com>\r\n"},
		{"EHLO test\r\n", "EHLO test\r\n"},
	}
	for _, tc := range cases {
		got := c.applyWorkarounds(tc.in)
		if got != tc.want {
			t.Errorf("applyWorkarounds(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyWorkarounds_Both(t *testing.T) {
	c := &workaroundConn{workarounds: workaroundWhitespaceBeforePath | workaroundMailboxForPath}
	cases := []struct{ in, want string }{
		{"MAIL FROM: alice@example.com\r\n", "MAIL FROM:<alice@example.com>\r\n"},
		{"RCPT TO:  bob\r\n", "RCPT TO:<bob>\r\n"},
	}
	for _, tc := range cases {
		got := c.applyWorkarounds(tc.in)
		if got != tc.want {
			t.Errorf("applyWorkarounds(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWorkaround_Integration(t *testing.T) {
	opts := Options{
		Config: config.SubmissionProtocolConfig{
			Hostname:    "mx.example.com",
			MaxMsgSize:  1 << 20,
			Workarounds: []string{"whitespace-before-path"},
		},
		Auth: stubAuth{},
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	readLine := func() string {
		t.Helper()
		sc.Scan()
		return sc.Text()
	}
	send := func(line string) {
		t.Helper()
		fmt.Fprintf(conn, "%s\r\n", line)
	}

	readLine() // 220 greeting
	send("EHLO test")
	for {
		if strings.HasPrefix(readLine(), "250 ") {
			break
		}
	}

	// AUTH PLAIN: base64("\x00alice@example.com\x00secret")
	send("AUTH PLAIN AGFsaWNlQGV4YW1wbGUuY29tAHNlY3JldA==")
	if resp := readLine(); !strings.HasPrefix(resp, "235") {
		t.Fatalf("AUTH: expected 235, got %q", resp)
	}

	send("MAIL FROM: <alice@example.com>")
	if resp := readLine(); !strings.HasPrefix(resp, "250") {
		t.Fatalf("MAIL FROM with space: expected 250, got %q", resp)
	}

	send("RCPT TO: <bob@example.com>")
	if resp := readLine(); !strings.HasPrefix(resp, "250") {
		t.Fatalf("RCPT TO with space: expected 250, got %q", resp)
	}
}

// A name the parser does not know must come back, not vanish.
//
// It used to vanish, and one of the vanishing names was published: the config
// comment listed `implicit-auth-external` beside the two real ones, so an
// operator could copy a value straight out of our own documentation, have it
// accepted, and watch nothing happen. A typo did the same thing, quieter.
//
// The unknown names are returned rather than logged from inside, so this row
// can assert them without capturing a log handler -- the caller logs.
func TestUnknownWorkaroundsComeBack(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		wantMask    submissionWorkarounds
		wantUnknown []string
	}{
		{
			name:     "the published name that was never implemented",
			input:    []string{"implicit-auth-external"},
			wantMask: 0, wantUnknown: []string{"implicit-auth-external"},
		},
		{
			name:     "a typo keeps the working ones working",
			input:    []string{"whitespace-before-path", "mailbox-for-paths"},
			wantMask: workaroundWhitespaceBeforePath, wantUnknown: []string{"mailbox-for-paths"},
		},
		{
			name:     "known names report nothing",
			input:    []string{"whitespace-before-path", "MAILBOX-FOR-PATH"},
			wantMask: workaroundWhitespaceBeforePath | workaroundMailboxForPath, wantUnknown: nil,
		},
		{
			// An empty entry is a stray comma or a blank list item, not a
			// misspelled setting: reporting it would train the operator to
			// ignore the warning.
			name:  "an empty entry is not a mistake worth reporting",
			input: []string{"", "  "},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mask, unknown := parseWorkarounds(tc.input)
			if mask != tc.wantMask {
				t.Errorf("mask = %b, want %b", mask, tc.wantMask)
			}
			if len(unknown) != len(tc.wantUnknown) {
				t.Fatalf("unknown = %v, want %v", unknown, tc.wantUnknown)
			}
			for i := range tc.wantUnknown {
				if unknown[i] != tc.wantUnknown[i] {
					t.Errorf("unknown[%d] = %q, want %q", i, unknown[i], tc.wantUnknown[i])
				}
			}
		})
	}
}

// The warning names what the operator could have meant, so the accepted set
// has to be the accepted set -- a stale list here would send them to a name
// that also does nothing.
func TestKnownWorkaroundsMatchesTheParser(t *testing.T) {
	for _, name := range knownWorkarounds() {
		mask, unknown := parseWorkarounds([]string{name})
		if mask == 0 || len(unknown) != 0 {
			t.Errorf("knownWorkarounds() offers %q, which the parser does not accept", name)
		}
	}
}
