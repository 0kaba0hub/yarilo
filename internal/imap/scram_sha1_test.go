package imap_test

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SCRAM-SHA-1 test driver.
	"encoding/base64"
	"fmt"
	"hash"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/pbkdf2"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// scramSha1Auth mirrors scramAuth (SHA-256) — implements both
// Authenticator (stub that never succeeds) AND SCRAMSha1Lookup.
type scramSha1Auth struct {
	user  string
	creds *sasl.ScramCredentials
}

func (a *scramSha1Auth) Authenticate(_, _, _ string) (*protocol.AuthResponse, error) {
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func (a *scramSha1Auth) LookupSCRAMSha1(user string) (*sasl.ScramCredentials, error) {
	if user == a.user {
		return a.creds, nil
	}
	return nil, nil
}

func rawIMAPSCRAMSha1(t *testing.T, conn net.Conn, user, pass string) (bool, error) {
	t.Helper()
	br := bufio.NewReader(conn)
	clientNonce := "rOprNGfwEbeRWgbNEkqO"

	clientFirstBare := fmt.Sprintf("n=%s,r=%s", user, clientNonce)
	clientFirst := "n,," + clientFirstBare
	enc := base64.StdEncoding.EncodeToString([]byte(clientFirst))
	fmt.Fprintf(conn, "a1 AUTHENTICATE SCRAM-SHA-1 %s\r\n", enc)

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
		return false, err
	}
	serverFirst := string(serverFirstBytes)

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

	cb := []byte("n,,")
	cbB64 := base64.StdEncoding.EncodeToString(cb)
	clientFinalWithoutProof := fmt.Sprintf("c=%s,r=%s", cbB64, combinedNonce)

	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	salted := pbkdf2.Key([]byte(pass), salt, iter, sha1.Size, sha1.New)
	clientKey := hmacSumSha1(sha1.New, salted, []byte("Client Key"))
	storedKey := sha1Sum(clientKey)
	clientSig := hmacSumSha1(sha1.New, storedKey, []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	clientFinal := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	enc = base64.StdEncoding.EncodeToString([]byte(clientFinal))
	fmt.Fprintf(conn, "%s\r\n", enc)

	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return false, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "+ ") {
			fmt.Fprintf(conn, "\r\n")
			continue
		}
		if strings.HasPrefix(line, "a1 OK") {
			return true, nil
		}
		if strings.HasPrefix(line, "a1 NO") || strings.HasPrefix(line, "a1 BAD") {
			return false, fmt.Errorf("server rejected: %s", line)
		}
	}
}

func hmacSumSha1(newHash func() hash.Hash, key, data []byte) []byte {
	m := hmac.New(newHash, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha1Sum(data []byte) []byte {
	h := sha1.Sum(data) //nolint:gosec // SCRAM-SHA-1.
	return h[:]
}

func TestIMAP_SCRAMSha1_RawWire(t *testing.T) {
	creds, _ := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramSha1Auth{user: "alice@example.com", creds: creds}

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
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	ok, err := rawIMAPSCRAMSha1(t, conn, "alice@example.com", "hunter2")
	if err != nil {
		t.Fatalf("SCRAM exchange: %v", err)
	}
	if !ok {
		t.Fatalf("SCRAM did not produce OK")
	}
}

func TestIMAP_SCRAMSha1_WrongPasswordRejected(t *testing.T) {
	creds, _ := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramSha1Auth{user: "alice@example.com", creds: creds}

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

	_, err := rawIMAPSCRAMSha1(t, conn, "alice@example.com", "WRONG")
	if err == nil {
		t.Errorf("expected SCRAM rejection on wrong password")
	}
}

func TestIMAP_SCRAMSha1_CapabilityAdvertised(t *testing.T) {
	creds, _ := sasl.GenerateScramSha1Credentials("hunter2", sasl.MinScramIterations)
	auth := &scramSha1Auth{user: "alice@example.com", creds: creds}
	c := startSCRAMServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for cap := range caps {
		if bytes.Contains([]byte(cap), []byte("SCRAM-SHA-1")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SCRAM-SHA-1 not advertised: %v", caps)
	}
}
