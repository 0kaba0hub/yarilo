package oauth2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksHarness owns a generated RSA keypair, the matching JWKS
// HTTP endpoint, and the helpers to sign tokens with it. Every
// local-validation test spins one up in t.TempDir-style scope.
type jwksHarness struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newJWKSHarness(t *testing.T) *jwksHarness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-kid"

	// Manually construct a single-key JWKS JSON document so the
	// test doesn't depend on the wider jwkset library beyond what
	// LocalJWTValidator consumes.
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}) // 65537
	jwks := map[string]interface{}{
		"keys": []map[string]interface{}{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": e},
		},
	}
	body, err := json.Marshal(jwks)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(server.Close)
	return &jwksHarness{key: key, server: server}
}

// sign serialises claims into a signed JWT with the harness key
// and the kid that matches the JWKS entry.
func (h *jwksHarness) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test-kid"
	signed, err := tok.SignedString(h.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// TestLocalJWT_HappyPath — well-formed token from a known JWKS
// passes all default checks.
func TestLocalJWT_HappyPath(t *testing.T) {
	h := newJWKSHarness(t)
	v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := h.sign(t, jwt.MapClaims{
		"iss":   "https://issuer.example",
		"sub":   "12345",
		"email": "alice@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"aud":   "yarilo",
	})
	c, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice@example.com" {
		t.Errorf("username = %q, want alice@example.com", c.Username)
	}
	if c.Subject != "12345" {
		t.Errorf("subject = %q", c.Subject)
	}
	if !c.Active {
		t.Errorf("active = false on signed non-expired token")
	}
}

// TestLocalJWT_ExpiredRejected — token whose exp is in the past
// returns ErrTokenExpired.
func TestLocalJWT_ExpiredRejected(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL:     h.server.URL,
		ExpireGrace: 1 * time.Second, // tight grace so test is fast
	})
	token := h.sign(t, jwt.MapClaims{
		"exp":   time.Now().Add(-time.Hour).Unix(),
		"email": "x@y",
	})
	_, err := v.Validate(context.Background(), token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want wrap of ErrTokenExpired", err)
	}
}

// TestLocalJWT_GraceWindowAllowsRecentlyExpired — token expired
// within ExpireGrace still passes (clock-skew tolerance).
func TestLocalJWT_GraceWindowAllowsRecentlyExpired(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL:     h.server.URL,
		ExpireGrace: 5 * time.Minute,
	})
	token := h.sign(t, jwt.MapClaims{
		"exp":   time.Now().Add(-30 * time.Second).Unix(), // 30s past, within 5m grace
		"email": "x@y",
	})
	_, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Errorf("token within grace rejected: %v", err)
	}
}

// TestLocalJWT_IssuerCheck — issuer not in allowed list rejects.
func TestLocalJWT_IssuerCheck(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL,
		Issuers: []string{"https://expected.example"},
	})
	token := h.sign(t, jwt.MapClaims{
		"iss":   "https://attacker.example",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "x@y",
	})
	_, err := v.Validate(context.Background(), token)
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Errorf("err = %v, want wrap of ErrIssuerMismatch", err)
	}
}

// TestLocalJWT_AudienceCheck — audience mismatch rejects, match passes.
func TestLocalJWT_AudienceCheck(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL:  h.server.URL,
		Audience: "yarilo",
	})
	good := h.sign(t, jwt.MapClaims{
		"aud": "yarilo", "exp": time.Now().Add(time.Hour).Unix(), "email": "x@y",
	})
	if _, err := v.Validate(context.Background(), good); err != nil {
		t.Errorf("matching audience rejected: %v", err)
	}
	bad := h.sign(t, jwt.MapClaims{
		"aud": "other", "exp": time.Now().Add(time.Hour).Unix(), "email": "x@y",
	})
	if _, err := v.Validate(context.Background(), bad); !errors.Is(err, ErrAudienceMismatch) {
		t.Errorf("err = %v, want wrap of ErrAudienceMismatch", err)
	}
}

// TestLocalJWT_AudienceArray — JWT spec allows aud to be a string
// array. Match against any entry passes.
func TestLocalJWT_AudienceArray(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL, Audience: "yarilo",
	})
	token := h.sign(t, jwt.MapClaims{
		"aud":   []string{"client-app", "yarilo", "another-service"},
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "x@y",
	})
	if _, err := v.Validate(context.Background(), token); err != nil {
		t.Errorf("audience array containing wanted value rejected: %v", err)
	}
}

// TestLocalJWT_RequiredScopes — every required scope must be
// present in the space-separated scope claim.
func TestLocalJWT_RequiredScopes(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL:        h.server.URL,
		RequiredScopes: []string{"mail.read", "mail.send"},
	})
	good := h.sign(t, jwt.MapClaims{
		"scope": "openid mail.send extra mail.read",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "x@y",
	})
	if _, err := v.Validate(context.Background(), good); err != nil {
		t.Errorf("all scopes present yet rejected: %v", err)
	}
	bad := h.sign(t, jwt.MapClaims{
		"scope": "openid mail.send",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "x@y",
	})
	if _, err := v.Validate(context.Background(), bad); !errors.Is(err, ErrScopeMissing) {
		t.Errorf("err = %v, want wrap of ErrScopeMissing", err)
	}
}

// TestLocalJWT_BadSignature — token signed with a different key
// fails signature verification.
func TestLocalJWT_BadSignature(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL,
	})
	// Forge a token with a fresh, unrelated key.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(), "email": "x@y",
	})
	tok.Header["kid"] = "test-kid"
	forged, _ := tok.SignedString(other)
	if _, err := v.Validate(context.Background(), forged); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("forged token accepted: err=%v", err)
	}
}

// TestLocalJWT_CustomUsernameAttribute — username_attribute knob
// resolves a non-default claim name (e.g. preferred_username).
func TestLocalJWT_CustomUsernameAttribute(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL:           h.server.URL,
		UsernameAttribute: "preferred_username",
	})
	token := h.sign(t, jwt.MapClaims{
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"exp":                time.Now().Add(time.Hour).Unix(),
	})
	c, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice" {
		t.Errorf("username = %q, want alice", c.Username)
	}
}

// TestLocalJWT_ExtraClaims — any non-mandatory claim lands in
// Claims.Extra so the passdb can surface it downstream.
func TestLocalJWT_ExtraClaims(t *testing.T) {
	h := newJWKSHarness(t)
	v, _ := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL,
	})
	token := h.sign(t, jwt.MapClaims{
		"email":   "x@y",
		"exp":     time.Now().Add(time.Hour).Unix(),
		"org_id":  "tenant-42",
		"is_paid": true,
		"uid":     1001,
	})
	c, _ := v.Validate(context.Background(), token)
	if c.Extra["org_id"] != "tenant-42" {
		t.Errorf("org_id = %q", c.Extra["org_id"])
	}
	if c.Extra["is_paid"] != "true" {
		t.Errorf("is_paid = %q", c.Extra["is_paid"])
	}
	if c.Extra["uid"] != "1001" {
		t.Errorf("uid = %q", c.Extra["uid"])
	}
}

// TestNewLocalJWTValidator_RequiresURL — empty JWKSURL returns
// error at construction.
func TestNewLocalJWTValidator_RequiresURL(t *testing.T) {
	_, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{})
	if err == nil {
		t.Errorf("empty JWKSURL accepted")
	}
}

// TestNewLocalJWTValidator_UpstreamUnreachable — JWKSURL pointing
// at a closed port wraps ErrUpstream so the operator can
// distinguish config typo from value error.
func TestNewLocalJWTValidator_UpstreamUnreachable(t *testing.T) {
	_, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: "http://127.0.0.1:1/.well-known/jwks.json",
	})
	if err == nil {
		t.Skip("unreachable JWKS unexpectedly succeeded; skipping (network flake)")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want wrap of ErrUpstream", err)
	}
}
