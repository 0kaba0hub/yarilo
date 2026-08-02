package imap_test

import (
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// oauthBearerAuth is a test double that accepts a single
// (username, token) pair. The OAUTHBEARER session callback calls
// .Authenticate with token in the password slot — token comparison
// happens here.
type oauthBearerAuth struct {
	user, token string
}

func (a *oauthBearerAuth) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if username == a.user && password == a.token {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func startOAuthServer(t *testing.T, auth protocol.Authenticator, enabled bool) *imapclient.Client {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	opts := imapserver.Options{
		Mailbox:       maildir.New(),
		Index:         file.New(),
		Resolver:      resolver,
		Auth:          auth,
		OAuth2Enabled: enabled,
	}
	srv := imapserver.New(opts)
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
		t.Fatalf("greeting: %v", err)
	}
	return c
}

// TestIMAP_OAuthBearer_Success — real OAUTHBEARER client sends
// (username, token); server validates and lets SELECT INBOX
// succeed.
func TestIMAP_OAuthBearer_Success(t *testing.T) {
	auth := &oauthBearerAuth{user: "alice@example.com", token: "valid-token"}
	c := startOAuthServer(t, auth, true)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "valid-token",
	})
	if err := c.Authenticate(saslClient); err != nil {
		t.Fatalf("AUTHENTICATE OAUTHBEARER: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX after OAUTHBEARER: %v", err)
	}
}

// TestIMAP_OAuthBearer_InvalidToken — bad token surfaces as
// SASL invalid_token error.
func TestIMAP_OAuthBearer_InvalidToken(t *testing.T) {
	auth := &oauthBearerAuth{user: "alice@example.com", token: "valid-token"}
	c := startOAuthServer(t, auth, true)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "WRONG",
	})
	if err := c.Authenticate(saslClient); err == nil {
		t.Fatalf("AUTHENTICATE OAUTHBEARER with bad token: expected error, got nil")
	}
}

// TestIMAP_OAuthBearer_DisabledNotAdvertised — when OAuth2Enabled
// is false, OAUTHBEARER is rejected even if the client tries it
// explicitly (defence-in-depth — clients should not see it
// advertised in the CAPABILITY reply either).
func TestIMAP_OAuthBearer_DisabledNotAdvertised(t *testing.T) {
	auth := &oauthBearerAuth{user: "alice@example.com", token: "valid-token"}
	c := startOAuthServer(t, auth, false)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	for cap := range caps {
		if strings.Contains(string(cap), "OAUTHBEARER") {
			t.Errorf("OAUTHBEARER advertised while disabled: %v", caps)
		}
	}

	saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "valid-token",
	})
	if err := c.Authenticate(saslClient); err == nil {
		t.Errorf("AUTHENTICATE OAUTHBEARER accepted while disabled")
	}
}

// TestIMAP_OAuthBearer_CapabilityAdvertised — when enabled, the
// AUTH=OAUTHBEARER capability is on the wire so clients can pick
// it.
func TestIMAP_OAuthBearer_CapabilityAdvertised(t *testing.T) {
	auth := &oauthBearerAuth{user: "alice@example.com", token: "valid-token"}
	c := startOAuthServer(t, auth, true)
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	found := false
	for cap := range caps {
		if strings.Contains(string(cap), "OAUTHBEARER") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OAUTHBEARER not advertised when enabled: %v", caps)
	}
}
