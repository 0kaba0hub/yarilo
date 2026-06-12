package pop3

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// masterMockAuth implements both Authenticator and
// MasterAuthenticator. AuthenticateMaster grants impersonation
// when (master,pass) matches and target is registered.
type masterMockAuth struct {
	users         map[string]string // username → password (for regular auth)
	masterUser    string
	masterPass    string
	allowedTarget map[string]bool
}

func (m *masterMockAuth) Authenticate(user, pass, _, _ string) (*protocol.AuthResponse, error) {
	if expected, ok := m.users[user]; ok && expected == pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: user}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func (m *masterMockAuth) AuthenticateMaster(authzid, authid, password, _, _ string) (*protocol.AuthResponse, error) {
	if authid != m.masterUser || password != m.masterPass {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	if !m.allowedTarget[authzid] {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthOK, Username: authzid}, nil
}

// TestSession_AuthPlain_MasterUserImpersonates — SASL PLAIN with
// authzid=alice authid=admin password=admin's-password. POP3 must
// authenticate the session as alice via the MasterAuthenticator
// path and land in TRANSACTION state.
func TestSession_AuthPlain_MasterUserImpersonates(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &masterMockAuth{
		masterUser:    "admin@test.com",
		masterPass:    "adminpass",
		allowedTarget: map[string]bool{"alice@test.com": true},
	}
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString(
		[]byte("alice@test.com\x00admin@test.com\x00adminpass"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("AUTH PLAIN master-user: expected +OK, got %q", resp)
	}
	// Session is authenticated; STAT must succeed.
	send(t, c, "STAT")
	if r2 := readline(t, r); !strings.HasPrefix(r2, "+OK") {
		t.Fatalf("STAT after master-user AUTH: expected +OK, got %q", r2)
	}
}

// TestSession_AuthPlain_MasterWrongPasswordRejected — same authzid
// but wrong master password.
func TestSession_AuthPlain_MasterWrongPasswordRejected(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &masterMockAuth{
		masterUser:    "admin@test.com",
		masterPass:    "adminpass",
		allowedTarget: map[string]bool{"alice@test.com": true},
	}
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString(
		[]byte("alice@test.com\x00admin@test.com\x00WRONG"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH PLAIN master-user wrong pass: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthPlain_UnregisteredTargetRejected — master
// authenticates but the target is not on the allow-list.
func TestSession_AuthPlain_UnregisteredTargetRejected(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &masterMockAuth{
		masterUser:    "admin@test.com",
		masterPass:    "adminpass",
		allowedTarget: map[string]bool{"alice@test.com": true},
	}
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString(
		[]byte("bob@test.com\x00admin@test.com\x00adminpass"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH PLAIN unregistered target: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthPlain_BackendWithoutMasterSupport — when the
// configured Authenticator is a plain Authenticator (no master
// surface), a non-empty distinct authzid is rejected outright.
func TestSession_AuthPlain_BackendWithoutMasterSupport(t *testing.T) {
	// mockAuth implements only Authenticator.
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"admin": "adminpass"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString(
		[]byte("alice\x00admin\x00adminpass"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH PLAIN authzid vs non-master backend: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthPlain_AuthzidEqualsAuthidIsRegularLogin — RFC 4616
// allows authzid == authid; behaves as a regular login.
func TestSession_AuthPlain_AuthzidEqualsAuthidIsRegularLogin(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString(
		[]byte("alice\x00alice\x00secret"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("AUTH PLAIN (authzid==authid): expected +OK, got %q", resp)
	}
}

// TestSession_UserPassNeverImpersonates — the USER/PASS legacy
// command path has no authzid surface; the session passes "" so
// the regular Authenticate path always runs.
func TestSession_UserPassNeverImpersonates(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &masterMockAuth{
		users:         map[string]string{"alice@test.com": "alicepass"},
		masterUser:    "admin@test.com",
		masterPass:    "adminpass",
		allowedTarget: map[string]bool{"alice@test.com": true},
	}
	c, r := newPOP3Session(t, opts)

	// admin sends USER admin + PASS adminpass. master flow MUST NOT
	// trigger from this path even though admin is a configured
	// master — USER/PASS has no authzid surface. So the regular
	// Authenticate is called with username=admin, which is NOT in
	// `users` → -ERR.
	send(t, c, "USER admin@test.com")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("USER: expected +OK, got %q", resp)
	}
	send(t, c, "PASS adminpass")
	resp = readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("PASS for admin via USER/PASS path: expected -ERR (admin not in regular users), got %q", resp)
	}
}
