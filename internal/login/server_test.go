package login

import (
	"bufio"
	"net"
	"strings"
	"testing"
)

// pipePair returns two connected net.Conns (server-side, client-side).
func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server := <-ch
	t.Cleanup(func() { client.Close(); server.Close() })
	return server, client
}

func TestExtractIMAPPreamble_Login(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, err := extractIMAPPreamble(srv, rd, nil)
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)

	// Read greeting.
	greeting, _ := crd.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	// Send LOGIN.
	cli.Write([]byte("A1 LOGIN alice \"secret123\"\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractIMAPPreamble: %v", err)
	}
	if got.username != "alice" {
		t.Errorf("username = %q, want %q", got.username, "alice")
	}
	if got.cmdTag != "A1" {
		t.Errorf("cmdTag = %q, want %q", got.cmdTag, "A1")
	}
	if len(got.authLines) != 1 || !strings.Contains(got.authLines[0], "LOGIN alice") {
		t.Errorf("authLines = %v, want LOGIN alice", got.authLines)
	}
}

func TestExtractIMAPPreamble_AuthenticatePlain(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, err := extractIMAPPreamble(srv, rd, nil)
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // greeting

	// AUTHENTICATE PLAIN with inline credentials: \0alice\0pass encoded.
	import64 := "AGFsaWNlAHBhc3M=" // base64("\x00alice\x00pass")
	cli.Write([]byte("B1 AUTHENTICATE PLAIN " + import64 + "\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractIMAPPreamble: %v", err)
	}
	if got.username != "alice" {
		t.Errorf("username = %q, want %q", got.username, "alice")
	}
	if got.cmdTag != "B1" {
		t.Errorf("cmdTag = %q, want %q", got.cmdTag, "B1")
	}
}

func TestExtractPOP3Preamble(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, err := extractPOP3Preamble(srv, rd, nil)
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // greeting

	cli.Write([]byte("USER bob\r\n"))
	crd.ReadString('\n') // +OK

	cli.Write([]byte("PASS secret\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractPOP3Preamble: %v", err)
	}
	if got.username != "bob" {
		t.Errorf("username = %q, want %q", got.username, "bob")
	}
	if len(got.authLines) != 2 {
		t.Fatalf("authLines len = %d, want 2", len(got.authLines))
	}
	if !strings.HasPrefix(got.authLines[0], "USER bob") {
		t.Errorf("authLines[0] = %q", got.authLines[0])
	}
	if !strings.HasPrefix(got.authLines[1], "PASS secret") {
		t.Errorf("authLines[1] = %q", got.authLines[1])
	}
}

func TestExtractSubmissionPreamble_Plain(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, err := extractSubmissionPreamble(srv, rd, nil)
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // 220 greeting

	cli.Write([]byte("EHLO client.example.com\r\n"))
	// Read multi-line EHLO response.
	for {
		line, _ := crd.ReadString('\n')
		if len(line) >= 4 && line[3] != '-' {
			break
		}
	}

	// AUTH PLAIN: \0carol\0pass
	import64 := "AGNhcm9sAHBhc3M=" // base64("\x00carol\x00pass")
	cli.Write([]byte("AUTH PLAIN " + import64 + "\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractSubmissionPreamble: %v", err)
	}
	if got.username != "carol" {
		t.Errorf("username = %q, want %q", got.username, "carol")
	}
	if len(got.authLines) != 1 || !strings.Contains(got.authLines[0], "AUTH PLAIN") {
		t.Errorf("authLines = %v", got.authLines)
	}
	if !strings.Contains(got.ehloLine, "client.example.com") {
		t.Errorf("ehloLine = %q", got.ehloLine)
	}
}

func TestDecodePlainUsername(t *testing.T) {
	cases := []struct {
		name    string
		b64     string
		wantU   string
		wantErr bool
	}{
		{
			name:  "authzid empty",
			b64:   "AGFsaWNlAHBhc3M=", // \x00alice\x00pass
			wantU: "alice",
		},
		{
			name:  "authzid set",
			b64:   "Ym9iAGJvYgBwYXNz", // bob\x00bob\x00pass
			wantU: "bob",
		},
		{
			name:    "invalid base64",
			b64:     "!!!",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := decodePlainUsername(tc.b64)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if u != tc.wantU {
				t.Errorf("username = %q, want %q", u, tc.wantU)
			}
		})
	}
}
