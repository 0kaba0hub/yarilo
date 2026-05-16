package login

import (
	"bufio"
	"crypto/tls"
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

func TestExtractIMAPPreamble_AuthenticateLogin(t *testing.T) {
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

	cli.Write([]byte("C1 AUTHENTICATE LOGIN\r\n"))

	// Read username challenge: "+ VXNlcm5hbWU6"
	challenge, _ := crd.ReadString('\n')
	if !strings.HasPrefix(challenge, "+") {
		t.Fatalf("expected username challenge, got %q", challenge)
	}
	// Send base64("alice")
	import64User := "YWxpY2U="
	cli.Write([]byte(import64User + "\r\n"))

	// Read password challenge: "+ UGFzc3dvcmQ6"
	challenge2, _ := crd.ReadString('\n')
	if !strings.HasPrefix(challenge2, "+") {
		t.Fatalf("expected password challenge, got %q", challenge2)
	}
	// Send base64("secret")
	import64Pass := "c2VjcmV0"
	cli.Write([]byte(import64Pass + "\r\n"))

	if err := <-errCh; err != nil {
		t.Fatalf("extractIMAPPreamble: %v", err)
	}
	if got.username != "alice" {
		t.Errorf("username = %q, want %q", got.username, "alice")
	}
	if got.cmdTag != "C1" {
		t.Errorf("cmdTag = %q, want %q", got.cmdTag, "C1")
	}
	// Backend gets AUTHENTICATE PLAIN (re-encoded from LOGIN credentials).
	if len(got.authLines) != 1 || !strings.Contains(got.authLines[0], "AUTHENTICATE PLAIN") {
		t.Errorf("authLines = %v, want AUTHENTICATE PLAIN", got.authLines)
	}
}

func TestExtractIMAPPreamble_StarttlsAdvertised(t *testing.T) {
	cases := []struct {
		name      string
		tlsCfg    *tls.Config // nil = no STARTTLS
		wantInCap bool
	}{
		{"with StarttlsTLS", &tls.Config{}, true},
		{"without StarttlsTLS", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := pipePair(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				rd := bufio.NewReader(srv)
				extractIMAPPreamble(srv, rd, tc.tlsCfg) //nolint:errcheck
			}()

			crd := bufio.NewReader(cli)
			crd.ReadString('\n') // greeting

			cli.Write([]byte("X1 CAPABILITY\r\n"))
			var capLine string
			for {
				line, _ := crd.ReadString('\n')
				if strings.HasPrefix(line, "* CAPABILITY") {
					capLine = line
				}
				if strings.HasPrefix(strings.TrimSpace(line), "X1 OK") {
					break
				}
			}

			hasSTARTTLS := strings.Contains(capLine, "STARTTLS")
			if hasSTARTTLS != tc.wantInCap {
				t.Errorf("STARTTLS in CAPABILITY = %v, want %v (cap line: %q)", hasSTARTTLS, tc.wantInCap, capLine)
			}
			cli.Close()
			<-done
		})
	}
}

func TestExtractIMAPPreamble_StarttlsUnavailable(t *testing.T) {
	// When StarttlsTLS is nil, sending STARTTLS must return NO (not drop conn).
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	go func() {
		rd := bufio.NewReader(srv)
		_, err := extractIMAPPreamble(srv, rd, nil)
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	crd.ReadString('\n') // greeting

	cli.Write([]byte("T1 STARTTLS\r\n"))
	resp, _ := crd.ReadString('\n')
	if !strings.HasPrefix(resp, "T1 NO") {
		t.Errorf("expected T1 NO, got %q", resp)
	}

	// Connection must still be alive; send LOGIN to complete.
	cli.Write([]byte("T2 LOGIN dave pass\r\n"))
	if err := <-errCh; err != nil {
		t.Fatalf("expected success after NO STARTTLS: %v", err)
	}
}

func TestExtractPOP3Preamble_AuthPlain(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string // AUTH PLAIN command sent by client
		inline   bool   // true = inline b64, false = challenge-response
		wantUser string
		wantPass string
	}{
		{
			name:     "challenge-response",
			cmd:      "AUTH PLAIN\r\n",
			inline:   false,
			wantUser: "eve",
			wantPass: "hunter2",
		},
		{
			name:     "inline",
			cmd:      "AUTH PLAIN AGV2ZQBodW50ZXIy\r\n", // \x00eve\x00hunter2
			inline:   true,
			wantUser: "eve",
			wantPass: "hunter2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			cli.Write([]byte(tc.cmd))
			if !tc.inline {
				// read "+ " challenge
				crd.ReadString('\n')
				// send base64("\x00eve\x00hunter2")
				cli.Write([]byte("AGV2ZQBodW50ZXIy\r\n"))
			}

			if err := <-errCh; err != nil {
				t.Fatalf("extractPOP3Preamble: %v", err)
			}
			if got.username != tc.wantUser {
				t.Errorf("username = %q, want %q", got.username, tc.wantUser)
			}
			if len(got.authLines) != 2 {
				t.Fatalf("authLines len = %d, want 2", len(got.authLines))
			}
			if !strings.HasPrefix(got.authLines[0], "USER "+tc.wantUser) {
				t.Errorf("authLines[0] = %q", got.authLines[0])
			}
			if !strings.HasPrefix(got.authLines[1], "PASS "+tc.wantPass) {
				t.Errorf("authLines[1] = %q", got.authLines[1])
			}
		})
	}
}

func TestExtractPOP3Preamble_StlsAdvertised(t *testing.T) {
	cases := []struct {
		name     string
		tlsCfg   *tls.Config
		wantSTLS bool
	}{
		{"with StarttlsTLS", &tls.Config{}, true},
		{"without StarttlsTLS", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := pipePair(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				rd := bufio.NewReader(srv)
				extractPOP3Preamble(srv, rd, tc.tlsCfg) //nolint:errcheck
			}()

			crd := bufio.NewReader(cli)
			crd.ReadString('\n') // greeting

			cli.Write([]byte("CAPA\r\n"))
			var capaLines []string
			for {
				line, _ := crd.ReadString('\n')
				capaLines = append(capaLines, strings.TrimRight(line, "\r\n"))
				if strings.TrimRight(line, "\r\n") == "." {
					break
				}
			}

			hasSTLS := false
			for _, l := range capaLines {
				if l == "STLS" {
					hasSTLS = true
					break
				}
			}
			if hasSTLS != tc.wantSTLS {
				t.Errorf("STLS in CAPA = %v, want %v (lines: %v)", hasSTLS, tc.wantSTLS, capaLines)
			}
			cli.Close()
			<-done
		})
	}
}

func TestDecodePlainCreds(t *testing.T) {
	cases := []struct {
		name     string
		b64      string
		wantUser string
		wantPass string
		wantErr  bool
	}{
		{
			name:     "authzid empty",
			b64:      "AGFsaWNlAHBhc3M=", // \x00alice\x00pass
			wantUser: "alice",
			wantPass: "pass",
		},
		{
			name:     "authzid set",
			b64:      "Ym9iAGJvYgBwYXNz", // bob\x00bob\x00pass
			wantUser: "bob",
			wantPass: "pass",
		},
		{
			name:    "invalid base64",
			b64:     "!!!",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, p, err := decodePlainCreds(tc.b64)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if u != tc.wantUser {
				t.Errorf("username = %q, want %q", u, tc.wantUser)
			}
			if p != tc.wantPass {
				t.Errorf("password = %q, want %q", p, tc.wantPass)
			}
		})
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
