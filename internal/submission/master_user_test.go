package submission

import (
	"net"
	"testing"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// stubMasterAuth implements both Authenticator and
// MasterAuthenticator. AuthPlainMaster grants impersonation
// when (master, password) matches and target is on the allow
// list.
type stubMasterAuth struct {
	regularUser, regularPass string
	masterUser, masterPass   string
	targets                  map[string]bool
}

func (a stubMasterAuth) AuthPlain(u, p string) error {
	if u == a.regularUser && p == a.regularPass {
		return nil
	}
	if u == a.masterUser && p == a.masterPass {
		return nil
	}
	return goSmtp.ErrAuthFailed
}

func (a stubMasterAuth) AuthPlainMaster(authzid, authid, password string) error {
	if authid != a.masterUser || password != a.masterPass {
		return goSmtp.ErrAuthFailed
	}
	if !a.targets[authzid] {
		return goSmtp.ErrAuthFailed
	}
	return nil
}

func buildMasterTestServer(t *testing.T, auth Authenticator) (string, func()) {
	t.Helper()
	opts := Options{
		Config: config.SubmissionProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		Auth: auth,
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, nil) }()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestSubmission_AuthPlain_MasterUserImpersonates — SASL PLAIN
// with authzid=alice authid=admin password=adminpass. The master
// flow accepts and the SMTP session continues as alice (verified
// indirectly: AUTH succeeds and MAIL FROM goes through).
func TestSubmission_AuthPlain_MasterUserImpersonates(t *testing.T) {
	auth := stubMasterAuth{
		masterUser: "admin@example.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@example.com": true},
	}
	addr, cleanup := buildMasterTestServer(t, auth)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("alice@example.com", "admin@example.com", "adminpass")
	if err := c.Auth(saslClient); err != nil {
		t.Fatalf("AUTH PLAIN master-user: %v", err)
	}
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL FROM after master AUTH: %v", err)
	}
}

// TestSubmission_AuthPlain_MasterWrongPasswordRejected — wrong
// master password reaches the master flow and is rejected
// opaquely.
func TestSubmission_AuthPlain_MasterWrongPasswordRejected(t *testing.T) {
	auth := stubMasterAuth{
		masterUser: "admin@example.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@example.com": true},
	}
	addr, cleanup := buildMasterTestServer(t, auth)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("alice@example.com", "admin@example.com", "WRONG")
	if err := c.Auth(saslClient); err == nil {
		t.Fatalf("AUTH PLAIN master wrong pass: expected error, got nil")
	}
}

// TestSubmission_AuthPlain_UnregisteredTargetRejected — master
// authenticates but the target is not in the allow-list.
func TestSubmission_AuthPlain_UnregisteredTargetRejected(t *testing.T) {
	auth := stubMasterAuth{
		masterUser: "admin@example.com", masterPass: "adminpass",
		targets: map[string]bool{"alice@example.com": true},
	}
	addr, cleanup := buildMasterTestServer(t, auth)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("bob@example.com", "admin@example.com", "adminpass")
	if err := c.Auth(saslClient); err == nil {
		t.Fatalf("AUTH PLAIN unregistered target: expected error, got nil")
	}
}

// TestSubmission_AuthPlain_NoAuthzidIsRegularLogin — empty
// authzid goes through the plain Authenticator path.
func TestSubmission_AuthPlain_NoAuthzidIsRegularLogin(t *testing.T) {
	auth := stubMasterAuth{
		masterUser: "admin@example.com", masterPass: "adminpass",
	}
	addr, cleanup := buildMasterTestServer(t, auth)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("", "admin@example.com", "adminpass")
	if err := c.Auth(saslClient); err != nil {
		t.Fatalf("AUTH PLAIN (no authzid): %v", err)
	}
}

// TestSubmission_AuthPlain_AuthzidEqualsAuthidIsRegularLogin —
// RFC 4616 permits authzid == authid; treat as regular login.
func TestSubmission_AuthPlain_AuthzidEqualsAuthidIsRegularLogin(t *testing.T) {
	auth := stubMasterAuth{
		regularUser: "alice@example.com", regularPass: "alicepass",
	}
	addr, cleanup := buildMasterTestServer(t, auth)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("alice@example.com", "alice@example.com", "alicepass")
	if err := c.Auth(saslClient); err != nil {
		t.Fatalf("AUTH PLAIN (authzid==authid): %v", err)
	}
}

// TestSubmission_AuthPlain_BackendWithoutMasterSupport — stubAuth
// only implements Authenticator; a distinct authzid must be
// rejected opaquely.
func TestSubmission_AuthPlain_BackendWithoutMasterSupport(t *testing.T) {
	// stubAuth only implements Authenticator (no AuthPlainMaster).
	addr, cleanup := buildMasterTestServer(t, stubAuth{})
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewPlainClient("bob@example.com", "alice@example.com", "secret")
	if err := c.Auth(saslClient); err == nil {
		t.Fatalf("AUTH PLAIN with authzid against non-master backend: expected error, got nil")
	}
}
