package imap_test

import (
	"net"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// masterAuth is a test double that implements both Authenticator and
// MasterAuthenticator. AuthenticateMaster grants impersonation when
// (master,pass) matches and the target is registered.
type masterAuth struct {
	regularUser, regularPass string
	masterUser, masterPass   string
	targets                  map[string]bool
}

func (m *masterAuth) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if username == m.regularUser && password == m.regularPass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	if username == m.masterUser && password == m.masterPass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func (m *masterAuth) AuthenticateMaster(authzid, authid, password, _, _ string) (*protocol.AuthResponse, error) {
	if authid != m.masterUser || password != m.masterPass {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	if !m.targets[authzid] {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthOK, Username: authzid}, nil
}

func startMasterServer(t *testing.T, auth protocol.Authenticator) *imapclient.Client {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: resolver,
		Auth:     auth,
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	return c
}

// TestAuthenticatePlain_MasterUserImpersonates — master sends an
// AUTHENTICATE PLAIN response with authzid=alice, authid=admin,
// password=admin's password. Expect: session is authenticated AS
// alice; SELECT INBOX must land in alice's home, not admin's.
func TestAuthenticatePlain_MasterUserImpersonates(t *testing.T) {
	auth := &masterAuth{
		regularUser: "alice@test.com", regularPass: "alicepass",
		masterUser: "admin@test.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@test.com": true},
	}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("alice@test.com", "admin@test.com", "adminpass")
	if err := c.Authenticate(saslClient); err != nil {
		t.Fatalf("AUTHENTICATE PLAIN with authzid: %v", err)
	}

	// After login, SELECT INBOX must succeed under alice's identity.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX after master impersonation: %v", err)
	}
}

// TestAuthenticatePlain_MasterWrongPasswordRejected — same authzid
// but wrong master password. Must fail with an opaque error.
func TestAuthenticatePlain_MasterWrongPasswordRejected(t *testing.T) {
	auth := &masterAuth{
		masterUser: "admin@test.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@test.com": true},
	}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("alice@test.com", "admin@test.com", "WRONG")
	if err := c.Authenticate(saslClient); err == nil {
		t.Fatalf("AUTHENTICATE PLAIN: expected error, got nil")
	}
}

// TestAuthenticatePlain_UnregisteredTargetRejected — master
// authenticates but the target is not in the allowed list.
// Reject without leaking which check failed.
func TestAuthenticatePlain_UnregisteredTargetRejected(t *testing.T) {
	auth := &masterAuth{
		masterUser: "admin@test.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@test.com": true},
	}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("bob@test.com", "admin@test.com", "adminpass")
	if err := c.Authenticate(saslClient); err == nil {
		t.Fatalf("AUTHENTICATE PLAIN: expected error for unregistered target, got nil")
	}
}

// TestAuthenticatePlain_NoAuthzidIsRegularLogin — authzid empty,
// authid=master. Should go through the regular login path (master
// logs in AS master, not impersonating).
func TestAuthenticatePlain_NoAuthzidIsRegularLogin(t *testing.T) {
	auth := &masterAuth{
		masterUser: "admin@test.com", masterPass: "adminpass",
	}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("", "admin@test.com", "adminpass")
	if err := c.Authenticate(saslClient); err != nil {
		t.Fatalf("AUTHENTICATE PLAIN (no authzid): %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX after self-login: %v", err)
	}
}

// TestAuthenticatePlain_BackendWithoutMasterSupport — when opts.Auth
// does NOT implement MasterAuthenticator, a distinct authzid must
// be rejected (matches go-imap's default-handler behaviour but
// applies even on our custom PLAIN handler).
func TestAuthenticatePlain_BackendWithoutMasterSupport(t *testing.T) {
	// stubPassdb implements only Authenticator (see server_test.go).
	auth := &stubPassdb{user: "user@test.com", pass: "testpass"}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("user@test.com", "admin@test.com", "anything")
	if err := c.Authenticate(saslClient); err == nil {
		t.Fatalf("AUTHENTICATE PLAIN with authzid: expected error from non-master backend, got nil")
	}
}

// TestAuthenticatePlain_AuthzidEqualsAuthidIsRegularLogin — RFC 4616
// permits the client to send authzid even when it equals authid; the
// server must treat this as a regular login, not impersonation.
func TestAuthenticatePlain_AuthzidEqualsAuthidIsRegularLogin(t *testing.T) {
	auth := &masterAuth{
		regularUser: "alice@test.com", regularPass: "alicepass",
	}
	c := startMasterServer(t, auth)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewPlainClient("alice@test.com", "alice@test.com", "alicepass")
	if err := c.Authenticate(saslClient); err != nil {
		t.Fatalf("AUTHENTICATE PLAIN (authzid==authid): %v", err)
	}
}
