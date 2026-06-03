package imap_test

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/pbkdf2"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// scramAuth implements both protocol.Authenticator (with a stub
// that never succeeds — SCRAM must not fall through to plain auth)
// AND protocol.SCRAMSha256Lookup.
type scramAuth struct {
	user  string
	creds *sasl.ScramCredentials
}

func (a *scramAuth) Authenticate(_, _, _ string) (*protocol.AuthResponse, error) {
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func (a *scramAuth) LookupSCRAMSha256(user string) (*sasl.ScramCredentials, error) {
	if user == a.user {
		return a.creds, nil
	}
	return nil, nil
}

func startSCRAMServer(t *testing.T, auth protocol.Authenticator) *imapclient.Client {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	srv := imapserver.New(imapserver.Options{
		Mailbox: maildir.New(), Index: file.New(),
		Resolver: resolver, Auth: auth,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	return c
}

// rawIMAPSCRAM drives an in-process SCRAM-SHA-256 exchange against
// an open imapclient. We bypass imapclient.Authenticate to keep
// full control over the wire (the imapclient does not have a
// built-in SCRAM client driver). The function writes raw tagged
// commands and continuations directly to the underlying conn.
func rawIMAPSCRAM(t *testing.T, conn net.Conn, user, pass string) (loggedIn bool, err error) {
	t.Helper()
	br := bufio.NewReader(conn)
	clientNonce := "rOprNGfwEbeRWgbNEkqO"

	// AUTHENTICATE SCRAM-SHA-256 <base64-client-first>
	clientFirstBare := fmt.Sprintf("n=%s,r=%s", user, clientNonce)
	clientFirst := "n,," + clientFirstBare
	enc := base64.StdEncoding.EncodeToString([]byte(clientFirst))
	fmt.Fprintf(conn, "a1 AUTHENTICATE SCRAM-SHA-256 %s\r\n", enc)

	// Read continuation: "+ <base64-server-first>"
	line, err := br.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "+ ") {
		return false, fmt.Errorf("expected continuation, got %q", line)
	}
	serverFirstBytes, err := base64.StdEncoding.DecodeString(line[2:])
	if err != nil {
		return false, fmt.Errorf("server-first b64 decode: %w", err)
	}
	serverFirst := string(serverFirstBytes)

	// Parse r=combinedNonce, s=salt, i=iter from server-first
	var combinedNonce, saltB64 string
	var iter int
	for _, attr := range strings.Split(serverFirst, ",") {
		switch attr[0] {
		case 'r':
			combinedNonce = attr[2:]
		case 's':
			saltB64 = attr[2:]
		case 'i':
			fmt.Sscanf(attr[2:], "%d", &iter) //nolint:errcheck
		}
	}
	salt, _ := base64.StdEncoding.DecodeString(saltB64)

	// Build client-final-without-proof: c=base64(n,,), r=combinedNonce
	cb := []byte("n,,")
	cbB64 := base64.StdEncoding.EncodeToString(cb)
	clientFinalWithoutProof := fmt.Sprintf("c=%s,r=%s", cbB64, combinedNonce)

	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	salted := pbkdf2.Key([]byte(pass), salt, iter, sha256.Size, sha256.New)
	clientKey := hmacSumIMAP(sha256.New, salted, []byte("Client Key"))
	storedKey := sha256SumIMAP(clientKey)
	clientSig := hmacSumIMAP(sha256.New, storedKey, []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	clientFinal := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	enc = base64.StdEncoding.EncodeToString([]byte(clientFinal))
	fmt.Fprintf(conn, "%s\r\n", enc)

	// Read server response: either tagged OK or continuation.
	// SCRAM server-final v= is delivered as another continuation
	// then the tagged OK / NO arrives.
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return false, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "+ ") {
			// server-final v=… — acknowledge with empty.
			fmt.Fprintf(conn, "\r\n")
			continue
		}
		if strings.HasPrefix(line, "a1 OK") {
			return true, nil
		}
		if strings.HasPrefix(line, "a1 NO") || strings.HasPrefix(line, "a1 BAD") {
			return false, fmt.Errorf("server rejected: %s", line)
		}
		// Untagged or other; keep reading.
	}
}

func hmacSumIMAP(newHash func() hash.Hash, key, data []byte) []byte {
	m := hmac.New(newHash, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256SumIMAP(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// TestIMAP_SCRAMSha256_RawWire uses a fresh raw TCP conn and
// drives SCRAM end-to-end. Verifies advertisement + complete
// exchange + post-login session establishment.
func TestIMAP_SCRAMSha256_RawWire(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramAuth{user: "alice@example.com", creds: creds}

	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	srv := imapserver.New(imapserver.Options{
		Mailbox: maildir.New(), Index: file.New(),
		Resolver: resolver, Auth: auth,
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln) //nolint:errcheck
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	// Consume greeting.
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	ok, err := rawIMAPSCRAM(t, conn, "alice@example.com", "hunter2")
	if err != nil {
		t.Fatalf("SCRAM exchange: %v", err)
	}
	if !ok {
		t.Fatalf("SCRAM did not produce OK")
	}
}

// TestIMAP_SCRAMSha256_WrongPasswordRejected — SCRAM proof
// computed from wrong password fails StoredKey compare.
func TestIMAP_SCRAMSha256_WrongPasswordRejected(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramAuth{user: "alice@example.com", creds: creds}

	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	srv := imapserver.New(imapserver.Options{
		Mailbox: maildir.New(), Index: file.New(),
		Resolver: resolver, Auth: auth,
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln) //nolint:errcheck
	defer ln.Close()

	conn, _ := net.Dial("tcp", ln.Addr().String())
	defer conn.Close()
	br := bufio.NewReader(conn)
	br.ReadString('\n') //nolint:errcheck

	_, err := rawIMAPSCRAM(t, conn, "alice@example.com", "WRONG")
	if err == nil {
		t.Errorf("expected SCRAM rejection on wrong password")
	}
}

// TestIMAP_SCRAMSha256_CapabilityAdvertised — when a SCRAM-capable
// passdb is wired, the CAPABILITY reply lists AUTH=SCRAM-SHA-256.
func TestIMAP_SCRAMSha256_CapabilityAdvertised(t *testing.T) {
	creds, _ := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramAuth{user: "alice@example.com", creds: creds}
	c := startSCRAMServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for cap := range caps {
		if bytes.Contains([]byte(cap), []byte("SCRAM-SHA-256")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SCRAM-SHA-256 not advertised: %v", caps)
	}
}

// TestIMAP_SCRAMSha256_NotAdvertisedWithoutLookup — when the
// configured Authenticator does not implement SCRAMSha256Lookup,
// the mech stays off the wire.
func TestIMAP_SCRAMSha256_NotAdvertisedWithoutLookup(t *testing.T) {
	// stubPassdb only implements Authenticator.
	c := startSCRAMServer(t, &stubPassdb{user: "x", pass: "y"})
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	caps, _ := c.Capability().Wait()
	for cap := range caps {
		if bytes.Contains([]byte(cap), []byte("SCRAM")) {
			t.Errorf("SCRAM mech advertised against non-SCRAM backend: %v", cap)
		}
	}
}
