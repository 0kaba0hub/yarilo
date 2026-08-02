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
		got := parseWorkarounds(tc.input)
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
