package director

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
)

// dialPipe returns two connected net.Conns via net.Pipe.
// server is passed to the preamble extractor; client simulates a mail client.
func dialPipe(t *testing.T) (server, client net.Conn) {
	t.Helper()
	server, client = net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	return
}

// readCRLFLine reads one CRLF-terminated line from r, returning it without the terminator.
func readCRLFLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readCRLFLine: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestExtractIMAPPreamble_Login(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractIMAPPreamble(srv, rd)
		got = p
		errCh <- err
	}()

	greeting := readCRLFLine(t, cliRd)
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("expected IMAP greeting, got %q", greeting)
	}

	fmt.Fprintf(cli, "A01 LOGIN user@example.com secret\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("extractIMAPPreamble: %v", err)
	}
	if got.username != "user@example.com" {
		t.Errorf("username = %q, want %q", got.username, "user@example.com")
	}
	if got.cmdTag != "A01" {
		t.Errorf("cmdTag = %q, want %q", got.cmdTag, "A01")
	}
	if !strings.Contains(got.authLine, "LOGIN user@example.com secret") {
		t.Errorf("authLine = %q, missing LOGIN command", got.authLine)
	}
}

func TestExtractIMAPPreamble_CapabilityThenLogin(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractIMAPPreamble(srv, rd)
		got = p
		errCh <- err
	}()

	readCRLFLine(t, cliRd) // greeting

	fmt.Fprintf(cli, "A01 CAPABILITY\r\n")
	for {
		line := readCRLFLine(t, cliRd)
		if strings.HasPrefix(line, "A01 OK") {
			break
		}
	}

	fmt.Fprintf(cli, "A02 LOGIN bob@domain.com pass123\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.username != "bob@domain.com" {
		t.Errorf("username = %q, want %q", got.username, "bob@domain.com")
	}
}

func TestExtractIMAPPreamble_AuthenticatePlain(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractIMAPPreamble(srv, rd)
		got = p
		errCh <- err
	}()

	readCRLFLine(t, cliRd) // greeting

	// AUTHENTICATE PLAIN with challenge-response
	fmt.Fprintf(cli, "A01 AUTHENTICATE PLAIN\r\n")
	challenge := readCRLFLine(t, cliRd)
	if challenge != "+ " {
		t.Fatalf("expected challenge, got %q", challenge)
	}

	// NUL authcid NUL passwd
	payload := base64.StdEncoding.EncodeToString([]byte("\x00alice@domain.com\x00secret"))
	fmt.Fprintf(cli, "%s\r\n", payload)

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.username != "alice@domain.com" {
		t.Errorf("username = %q, want %q", got.username, "alice@domain.com")
	}
}

func TestExtractIMAPPreamble_Logout(t *testing.T) {
	srv, cli := dialPipe(t)

	errCh := make(chan error, 1)
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		_, err := extractIMAPPreamble(srv, rd)
		errCh <- err
	}()

	// Run client in its own goroutine so net.Pipe reads/writes on both ends
	// can proceed concurrently without deadlocking.
	go func() {
		cliRd := bufio.NewReader(cli)
		cliRd.ReadString('\n')             //nolint:errcheck // greeting
		fmt.Fprintf(cli, "A01 LOGOUT\r\n") //nolint:errcheck
		cliRd.ReadString('\n')             //nolint:errcheck // * BYE
		cliRd.ReadString('\n')             //nolint:errcheck // A01 OK Logout
	}()

	if err := <-errCh; err == nil {
		t.Fatal("expected error on LOGOUT, got nil")
	}
}

func TestExtractPOP3Preamble_UserPass(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		p, err := extractPOP3Preamble(srv, rd)
		got = p
		errCh <- err
	}()

	greeting := readCRLFLine(t, cliRd)
	if !strings.HasPrefix(greeting, "+OK") {
		t.Fatalf("expected POP3 greeting, got %q", greeting)
	}

	fmt.Fprintf(cli, "USER carol@domain.com\r\n")
	readCRLFLine(t, cliRd) // +OK

	fmt.Fprintf(cli, "PASS secret123\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.username != "carol@domain.com" {
		t.Errorf("username = %q, want %q", got.username, "carol@domain.com")
	}
	if !strings.Contains(got.authLine, "USER carol@domain.com") {
		t.Errorf("authLine missing USER: %q", got.authLine)
	}
	if !strings.Contains(got.authLine, "PASS secret123") {
		t.Errorf("authLine missing PASS: %q", got.authLine)
	}
}

func TestExtractPOP3Preamble_PassWithoutUser(t *testing.T) {
	srv, cli := dialPipe(t)
	cliRd := bufio.NewReader(cli)

	errCh := make(chan error, 1)
	go func() {
		rd := bufio.NewReaderSize(srv, 4096)
		_, err := extractPOP3Preamble(srv, rd)
		errCh <- err
	}()

	readCRLFLine(t, cliRd)

	// PASS without prior USER
	fmt.Fprintf(cli, "PASS secret\r\n")
	errLine := readCRLFLine(t, cliRd)
	if !strings.HasPrefix(errLine, "-ERR") {
		t.Errorf("expected -ERR, got %q", errLine)
	}

	// Now send USER + PASS properly
	fmt.Fprintf(cli, "USER dave@domain.com\r\n")
	readCRLFLine(t, cliRd)
	fmt.Fprintf(cli, "PASS pass\r\n")

	if err := <-errCh; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	// Read multi-line response
	for {
		line := readCRLFLine(t, cliRd)
		if !strings.HasPrefix(line, "250-") {
			break
		}
	}

	fmt.Fprintf(cli, "MAIL FROM:<sender@example.com>\r\n")
	readCRLFLine(t, cliRd) // 250 OK for MAIL FROM

	fmt.Fprintf(cli, "RCPT TO:<eve@domain.com>\r\n")
	// Server returns preamble on RCPT TO without writing a response to client.

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

func TestDecodePlainAuth(t *testing.T) {
	cases := []struct {
		name    string
		payload string // NUL-delimited: [authzid] NUL authcid NUL passwd
		want    string
		wantErr bool
	}{
		{
			name:    "no authzid",
			payload: base64.StdEncoding.EncodeToString([]byte("\x00user@domain.com\x00secret")),
			want:    "user@domain.com",
		},
		{
			name:    "with authzid",
			payload: base64.StdEncoding.EncodeToString([]byte("proxy\x00user@domain.com\x00secret")),
			want:    "user@domain.com",
		},
		{name: "bad base64", payload: "!!!not-b64!!!", wantErr: true},
		{
			name:    "empty authcid",
			payload: base64.StdEncoding.EncodeToString([]byte("\x00\x00secret")),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodePlainAuth(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
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
