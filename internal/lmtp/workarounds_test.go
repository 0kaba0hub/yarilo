package lmtp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestLMTPWorkaroundConn_ApplyWorkarounds(t *testing.T) {
	cases := []struct {
		name        string
		workarounds lmtpWorkarounds
		in          string
		want        string
	}{
		{
			name:        "whitespace-before-path strips space",
			workarounds: workaroundWhitespaceBeforePath,
			in:          "MAIL FROM: <sender@ext.com>\r\n",
			want:        "MAIL FROM:<sender@ext.com>\r\n",
		},
		{
			name:        "whitespace-before-path strips space RCPT",
			workarounds: workaroundWhitespaceBeforePath,
			in:          "RCPT TO:  <alice@example.com>\r\n",
			want:        "RCPT TO:<alice@example.com>\r\n",
		},
		{
			name:        "mailbox-for-path wraps bare address",
			workarounds: workaroundMailboxForPath,
			in:          "RCPT TO:alice@example.com\r\n",
			want:        "RCPT TO:<alice@example.com>\r\n",
		},
		{
			name:        "both: space and bare address",
			workarounds: workaroundWhitespaceBeforePath | workaroundMailboxForPath,
			in:          "MAIL FROM: sender@ext.com\r\n",
			want:        "MAIL FROM:<sender@ext.com>\r\n",
		},
		{
			name:        "no workarounds: passthrough",
			workarounds: 0,
			in:          "MAIL FROM: <sender@ext.com>\r\n",
			want:        "MAIL FROM: <sender@ext.com>\r\n",
		},
		{
			name:        "non-envelope command unchanged",
			workarounds: workaroundWhitespaceBeforePath | workaroundMailboxForPath,
			in:          "LHLO postfix.test\r\n",
			want:        "LHLO postfix.test\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &lmtpWorkaroundConn{workarounds: tc.workarounds}
			got := c.applyWorkarounds(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLMTP_WhitespaceBeforePath_Integration delivers a message through an LMTP
// server with whitespace-before-path enabled, sending raw MAIL FROM with a space.
func TestLMTP_WhitespaceBeforePath_Integration(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := fileindex.New()

	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	box.Close() //nolint:errcheck

	srv := New(Options{
		Hostname: "lmtp.test",
		Config: config.LMTPProtocolConfig{
			ReadTimeout:        5,
			WriteTimeout:       5,
			HdrDeliveryAddress: "final",
			ClientWorkarounds:  []string{"whitespace-before-path"},
		},
		Mailbox:  mb,
		Index:    idx,
		Resolver: resolver,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	sc.Scan() // 220 greeting

	fmt.Fprintf(conn, "LHLO postfix.test\r\n")
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "250 ") {
			break
		}
	}

	// Send MAIL FROM with a space before < — workaround should fix it
	fmt.Fprintf(conn, "MAIL FROM: <sender@external.com>\r\n")
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("MAIL FROM with space: expected 250, got %q", sc.Text())
	}

	fmt.Fprintf(conn, "RCPT TO: <alice@example.com>\r\n")
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("RCPT TO with space: expected 250, got %q", sc.Text())
	}

	fmt.Fprintf(conn, "DATA\r\n")
	sc.Scan() // 354
	fmt.Fprintf(conn, "%s\r\n.\r\n", testMsg)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("DATA delivery: expected 250, got %q", sc.Text())
	}
}
