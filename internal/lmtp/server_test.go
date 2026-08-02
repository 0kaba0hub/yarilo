package lmtp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func buildTestServer(t *testing.T) string {
	t.Helper()
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
			AddReceivedHeader:  true,
			HdrDeliveryAddress: "final",
			ReadTimeout:        5,
			WriteTimeout:       5,
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
	return ln.Addr().String()
}

// dialLMTP connects and reads the greeting line, returns conn + scanner.
func dialLMTP(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	sc := bufio.NewScanner(conn)
	sc.Scan() // 220 greeting
	return conn, sc
}

// sendLHLO sends LHLO and reads capability lines until final "250 ".
func sendLHLO(t *testing.T, conn io.Writer, sc *bufio.Scanner) {
	t.Helper()
	fmt.Fprintf(conn, "LHLO postfix.test\r\n")
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "250 ") {
			return
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatalf("unexpected LHLO response: %q", line)
		}
	}
}

// deliver sends a complete LMTP transaction and returns per-recipient response lines.
func deliver(t *testing.T, conn net.Conn, sc *bufio.Scanner, from, to, msg string) []string {
	t.Helper()
	fmt.Fprintf(conn, "MAIL FROM:<%s>\r\n", from)
	sc.Scan()
	fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", to)
	sc.Scan()
	fmt.Fprintf(conn, "DATA\r\n")
	sc.Scan() // 354
	fmt.Fprintf(conn, "%s\r\n.\r\n", msg)

	var responses []string
	sc.Scan()
	responses = append(responses, sc.Text())
	return responses
}

const testMsg = "From: sender@external.com\r\nTo: alice@example.com\r\nSubject: hello\r\n\r\nHello\r\n"

func TestLMTP_Deliver(t *testing.T) {
	addr := buildTestServer(t)
	conn, sc := dialLMTP(t, addr)
	sendLHLO(t, conn, sc)

	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250, got: %q", resp[0])
	}
}

func TestLMTP_UnknownRecipient_AutoProvisions(t *testing.T) {
	// LMTP is trusted (behind an MTA); first delivery for a new recipient
	// auto-creates the Maildir instead of rejecting (LMTP trusted-delivery behaviour).
	addr := buildTestServer(t)
	conn, sc := dialLMTP(t, addr)
	sendLHLO(t, conn, sc)

	resp := deliver(t, conn, sc, "sender@external.com", "newcomer@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250 for first-time recipient (auto-provision), got: %q", resp[0])
	}
}

func TestLMTP_RejectsEHLO(t *testing.T) {
	addr := buildTestServer(t)
	conn, sc := dialLMTP(t, addr)

	fmt.Fprintf(conn, "EHLO test\r\n")
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "500") {
		t.Fatalf("expected 500 for EHLO on LMTP server, got: %q", sc.Text())
	}
}

func TestLMTP_MultipleRecipients(t *testing.T) {
	addr := buildTestServer(t)
	conn, sc := dialLMTP(t, addr)
	sendLHLO(t, conn, sc)

	fmt.Fprintf(conn, "MAIL FROM:<sender@external.com>\r\n")
	sc.Scan()
	fmt.Fprintf(conn, "RCPT TO:<alice@example.com>\r\n")
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("expected 250 for alice RCPT, got: %q", sc.Text())
	}
	fmt.Fprintf(conn, "RCPT TO:<bob@example.com>\r\n") // new user — auto-provisioned
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("expected 250 for bob RCPT (auto-provision), got: %q", sc.Text())
	}

	// DATA delivers per-recipient — two 250s expected (LMTP semantics).
	fmt.Fprintf(conn, "DATA\r\n")
	sc.Scan() // 354
	fmt.Fprintf(conn, "%s\r\n.\r\n", testMsg)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("expected 250 for alice delivery, got: %q", sc.Text())
	}
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("expected 250 for bob delivery, got: %q", sc.Text())
	}
}
