package director

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// dialPipe returns two connected net.Conns via net.Pipe.
func dialPipe(t *testing.T) (server, client net.Conn) {
	t.Helper()
	server, client = net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	return
}

func readCRLFLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readCRLFLine: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestExtractLMTPPreamble(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractLMTPPreamble(srv, rd)
		got = p
		errCh <- err
	}()

	fmt.Fprintf(cli, "LHLO mx.example.com\r\n")
	// Read multi-line 250 response.
	for {
		line := readCRLFLine(t, cliRd)
		if !strings.HasPrefix(line, "250-") {
			break
		}
	}

	fmt.Fprintf(cli, "MAIL FROM:<sender@example.com>\r\n")
	readCRLFLine(t, cliRd) // 250 OK for MAIL FROM

	fmt.Fprintf(cli, "RCPT TO:<eve@domain.com>\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.username != "eve@domain.com" {
		t.Errorf("username = %q, want %q", got.username, "eve@domain.com")
	}
	if !strings.Contains(got.authLine, "RCPT TO:<eve@domain.com>") {
		t.Errorf("authLine missing RCPT TO: %q", got.authLine)
	}
}

func TestExtractLMTPPreamble_Quit(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		_, err := extractLMTPPreamble(srv, rd)
		errCh <- err
	}()

	go func() {
		fmt.Fprintf(cli, "LHLO mx.example.com\r\n")
		for {
			line, _ := cliRd.ReadString('\n')
			if !strings.HasPrefix(line, "250-") {
				break
			}
		}
		fmt.Fprintf(cli, "QUIT\r\n")
		cliRd.ReadString('\n') //nolint:errcheck // 221
	}()

	if err := <-errCh; err == nil {
		t.Fatal("expected error on QUIT, got nil")
	}
}

func TestExtractLMTPPreamble_MalformedRCPT(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractLMTPPreamble(srv, rd)
		got = p
		errCh <- err
	}()

	fmt.Fprintf(cli, "LHLO mx.example.com\r\n")
	for {
		line := readCRLFLine(t, cliRd)
		if !strings.HasPrefix(line, "250-") {
			break
		}
	}
	fmt.Fprintf(cli, "MAIL FROM:<sender@example.com>\r\n")
	readCRLFLine(t, cliRd)

	// Malformed RCPT TO (no angle brackets) → 550 error, then valid RCPT TO.
	fmt.Fprintf(cli, "RCPT TO:noangles@domain.com\r\n")
	errLine := readCRLFLine(t, cliRd)
	if !strings.HasPrefix(errLine, "550") {
		t.Errorf("expected 550, got %q", errLine)
	}

	fmt.Fprintf(cli, "RCPT TO:<valid@domain.com>\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.username != "valid@domain.com" {
		t.Errorf("username = %q, want %q", got.username, "valid@domain.com")
	}
}
