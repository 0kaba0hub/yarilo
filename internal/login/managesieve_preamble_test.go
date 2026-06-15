package login

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// plainB64 encodes SASL PLAIN credentials as base64(\0user\0pass).
func plainB64(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
}

// readMSGreeting reads and returns all lines of the ManageSieve greeting
// up to and including the OK line.
func readMSGreeting(t *testing.T, rd *bufio.Reader) []string {
	t.Helper()
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("readMSGreeting: %v", err)
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if strings.HasPrefix(line, "OK") {
			break
		}
	}
	return lines
}

func TestExtractManageSievePreamble_AuthenticatePlainInline(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	greeting := readMSGreeting(t, crd)

	// Greeting must include IMPLEMENTATION and end with OK.
	if !containsPrefix(greeting, `"IMPLEMENTATION"`) {
		t.Fatalf("greeting missing IMPLEMENTATION: %v", greeting)
	}
	if !strings.HasPrefix(greeting[len(greeting)-1], "OK") {
		t.Fatalf("greeting must end with OK, got: %v", greeting)
	}

	b64 := plainB64("alice", "secret")
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\" %q\r\n", b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "alice" || got.password != "secret" {
		t.Fatalf("got user=%q pass=%q, want alice/secret", got.username, got.password)
	}
}

func TestExtractManageSievePreamble_AuthenticatePlainTwoStep(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	// AUTHENTICATE without initial response: server sends empty challenge.
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\"\r\n")

	// Server sends "" challenge.
	challenge, _ := crd.ReadString('\n')
	if strings.TrimRight(challenge, "\r\n") != `""` {
		t.Fatalf("expected empty challenge, got %q", challenge)
	}

	b64 := plainB64("bob", "hunter2")
	fmt.Fprintf(cli, "%q\r\n", b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "bob" || got.password != "hunter2" {
		t.Fatalf("got user=%q pass=%q, want bob/hunter2", got.username, got.password)
	}
}

func TestExtractManageSievePreamble_NoopThenAuth(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	fmt.Fprintf(cli, "NOOP\r\n")
	noop, _ := crd.ReadString('\n')
	if !strings.HasPrefix(noop, "OK") {
		t.Fatalf("expected OK for NOOP, got %q", noop)
	}

	b64 := plainB64("carol", "pass")
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\" %q\r\n", b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "carol" {
		t.Fatalf("want carol, got %q", got.username)
	}
}

func TestExtractManageSievePreamble_CapabilityThenAuth(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	fmt.Fprintf(cli, "CAPABILITY\r\n")
	// Server resends greeting; consume it.
	readMSGreeting(t, crd)

	b64 := plainB64("dave", "pw")
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\" %q\r\n", b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "dave" {
		t.Fatalf("want dave, got %q", got.username)
	}
}

func TestExtractManageSievePreamble_LogoutBeforeAuth(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	go func() {
		rd := bufio.NewReader(srv)
		_, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	fmt.Fprintf(cli, "LOGOUT\r\n")
	bye, _ := crd.ReadString('\n')
	if !strings.HasPrefix(bye, "BYE") {
		t.Fatalf("expected BYE, got %q", bye)
	}

	if err := <-errCh; err == nil {
		t.Fatal("expected error after LOGOUT, got nil")
	}
}

func TestExtractManageSievePreamble_UnknownCommand(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	fmt.Fprintf(cli, "BADCMD\r\n")
	no, _ := crd.ReadString('\n')
	if !strings.HasPrefix(no, "NO") {
		t.Fatalf("expected NO for unknown command, got %q", no)
	}

	b64 := plainB64("eve", "pw")
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\" %q\r\n", b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "eve" {
		t.Fatalf("want eve, got %q", got.username)
	}
}

func TestExtractManageSievePreamble_AuthLiteralArg(t *testing.T) {
	srv, cli := pipePair(t)
	errCh := make(chan error, 1)
	var got *preamble
	go func() {
		rd := bufio.NewReader(srv)
		p, _, _, err := extractManageSievePreamble(srv, rd, nil, Options{})
		got = p
		errCh <- err
	}()

	crd := bufio.NewReader(cli)
	readMSGreeting(t, crd)

	// AUTHENTICATE with a literal (non-synchronizing) initial response.
	b64 := plainB64("frank", "pw123")
	fmt.Fprintf(cli, "AUTHENTICATE \"PLAIN\" {%d+}\r\n%s\r\n", len(b64), b64)

	if err := <-errCh; err != nil {
		t.Fatalf("extractManageSievePreamble: %v", err)
	}
	if got.username != "frank" {
		t.Fatalf("want frank, got %q", got.username)
	}
}

func TestExtractManageSievePreamble_GreetingContainsVersion(t *testing.T) {
	srv, cli := pipePair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rd := bufio.NewReader(srv)
		extractManageSievePreamble(srv, rd, nil, Options{}) //nolint:errcheck
	}()

	crd := bufio.NewReader(cli)
	lines := readMSGreeting(t, crd)

	hasVersion := false
	for _, l := range lines {
		if strings.Contains(l, `"VERSION"`) && strings.Contains(l, `"1.0"`) {
			hasVersion = true
		}
	}
	if !hasVersion {
		t.Fatalf("greeting missing VERSION 1.0 capability: %v", lines)
	}
	cli.Close()
	<-done
}

func TestManageSieveGreeting_StarttlsAdvertised(t *testing.T) {
	srv, cli := pipePair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Minimal TLS config — just needs to be non-nil.
		extTLS := &tls.Config{} //nolint:gosec
		rd := bufio.NewReader(srv)
		extractManageSievePreamble(srv, rd, extTLS, Options{}) //nolint:errcheck
	}()

	crd := bufio.NewReader(cli)
	lines := readMSGreeting(t, crd)

	hasSTARTTLS := false
	for _, l := range lines {
		if l == `"STARTTLS"` {
			hasSTARTTLS = true
		}
	}
	if !hasSTARTTLS {
		t.Fatalf("expected STARTTLS in greeting when extTLS is non-nil: %v", lines)
	}
	cli.Close()
	<-done
}

// containsPrefix reports whether any element of lines has the given prefix.
func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
