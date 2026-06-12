package pop3

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// oauthBearerAuth is a test double that accepts a single
// (username, token) pair as valid OAUTHBEARER credentials.
type oauthBearerAuth struct {
	user, token string
}

func (a *oauthBearerAuth) Authenticate(user, pass, _, _ string) (*protocol.AuthResponse, error) {
	if user == a.user && pass == a.token {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: user}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// oauthBearerPayload assembles a base64-encoded OAUTHBEARER
// initial response with the supplied (username, token) pair.
func oauthBearerPayload(username, token string) string {
	raw := "n,a=" + username + ",\x01auth=Bearer " + token + "\x01\x01"
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// TestSession_AuthOAuthBearer_Success — real OAUTHBEARER session
// reaches TRANSACTION state, STAT works.
func TestSession_AuthOAuthBearer_Success(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &oauthBearerAuth{user: "alice@test.com", token: "valid-token"}
	opts.OAuth2Enabled = true
	c, r := newPOP3Session(t, opts)

	payload := oauthBearerPayload("alice@test.com", "valid-token")
	send(t, c, "AUTH OAUTHBEARER "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("AUTH OAUTHBEARER: expected +OK, got %q", resp)
	}
	send(t, c, "STAT")
	if r2 := readline(t, r); !strings.HasPrefix(r2, "+OK") {
		t.Fatalf("STAT after OAUTHBEARER: expected +OK, got %q", r2)
	}
}

// TestSession_AuthOAuthBearer_InvalidToken — wrong token surfaces
// as -ERR (no detail to attacker).
func TestSession_AuthOAuthBearer_InvalidToken(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &oauthBearerAuth{user: "alice@test.com", token: "valid-token"}
	opts.OAuth2Enabled = true
	c, r := newPOP3Session(t, opts)

	payload := oauthBearerPayload("alice@test.com", "WRONG")
	send(t, c, "AUTH OAUTHBEARER "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH OAUTHBEARER bad token: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthOAuthBearer_DisabledRejects — OAuth2Enabled=false
// rejects the mechanism outright.
func TestSession_AuthOAuthBearer_DisabledRejects(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &oauthBearerAuth{user: "alice@test.com", token: "valid-token"}
	opts.OAuth2Enabled = false
	c, r := newPOP3Session(t, opts)

	payload := oauthBearerPayload("alice@test.com", "valid-token")
	send(t, c, "AUTH OAUTHBEARER "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Errorf("AUTH OAUTHBEARER while disabled: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthOAuthBearer_InvalidBase64 — malformed payload
// gets the base64-error path.
func TestSession_AuthOAuthBearer_InvalidBase64(t *testing.T) {
	opts := newTestOpts(nil, &mockMailbox{}, &mockIndex{})
	opts.Auth = &oauthBearerAuth{user: "x", token: "y"}
	opts.OAuth2Enabled = true
	c, r := newPOP3Session(t, opts)

	send(t, c, "AUTH OAUTHBEARER not-valid-base64-!!!")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Errorf("AUTH OAUTHBEARER bad base64: expected -ERR, got %q", resp)
	}
}
