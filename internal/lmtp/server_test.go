package lmtp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

func buildTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)
	if err := mb.Init("alice@example.com"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck

	srv := New(Options{
		Hostname: "lmtp.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader:  true,
			HdrDeliveryAddress: "final",
			ReadTimeout:        5,
			WriteTimeout:       5,
		},
		Mailbox: mb,
		Index:   idx,
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

func TestLMTP_UnknownRecipient(t *testing.T) {
	addr := buildTestServer(t)
	conn, sc := dialLMTP(t, addr)
	sendLHLO(t, conn, sc)

	fmt.Fprintf(conn, "MAIL FROM:<sender@external.com>\r\n")
	sc.Scan()
	fmt.Fprintf(conn, "RCPT TO:<nobody@example.com>\r\n")
	sc.Scan()
	resp := sc.Text()
	if !strings.HasPrefix(resp, "550") {
		t.Fatalf("expected 550 5.1.1 at RCPT TO for unknown user, got: %q", resp)
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
	fmt.Fprintf(conn, "RCPT TO:<nobody@example.com>\r\n") // unknown — rejected at RCPT TO
	sc.Scan()
	rcptResp := sc.Text()
	if !strings.HasPrefix(rcptResp, "550") {
		t.Fatalf("expected 550 for unknown rcpt, got: %q", rcptResp)
	}

	// Only alice was accepted — DATA delivers to her only.
	fmt.Fprintf(conn, "DATA\r\n")
	sc.Scan() // 354
	fmt.Fprintf(conn, "%s\r\n.\r\n", testMsg)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("expected 250 for alice delivery, got: %q", sc.Text())
	}
}

func TestDeliverer_InProcess(t *testing.T) {
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)
	defer idx.Close() //nolint:errcheck
	if err := mb.Init("alice@example.com"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	d := NewDeliverer(mb, idx)
	results := d.Deliver(nil, "sender@external.com", []string{"alice@example.com"}, strings.NewReader(testMsg)) //nolint:staticcheck
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("delivery error: %v", results[0].Err)
	}
}
