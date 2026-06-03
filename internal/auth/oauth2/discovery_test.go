package oauth2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// discoveryHarness composes a JWKS endpoint, an introspection
// endpoint, and an OIDC document that points at them.
type discoveryHarness struct {
	jwks      *jwksHarness
	introsErr int // 0 → ok; non-zero status overrides
	introsBod string
	intros    *httptest.Server
	doc       *httptest.Server

	// Document fields tests control:
	docIssuer       string
	docJWKS         string
	docIntros       string
	emitJWKS        bool
	emitIntros      bool
	overrideDocBody string // when set, returned verbatim
}

func newDiscoveryHarness(t *testing.T) *discoveryHarness {
	t.Helper()
	dh := &discoveryHarness{
		jwks:       newJWKSHarness(t),
		introsBod:  `{"active":true,"email":"alice@example.com"}`,
		emitJWKS:   true,
		emitIntros: true,
	}
	dh.intros = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if dh.introsErr != 0 {
			w.WriteHeader(dh.introsErr)
		}
		w.Write([]byte(dh.introsBod)) //nolint:errcheck
	}))
	t.Cleanup(dh.intros.Close)

	dh.doc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		if dh.overrideDocBody != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(dh.overrideDocBody)) //nolint:errcheck
			return
		}
		dd := DiscoveryDocument{Issuer: dh.docIssuer}
		if dh.emitJWKS {
			if dh.docJWKS != "" {
				dd.JWKSURI = dh.docJWKS
			} else {
				dd.JWKSURI = dh.jwks.server.URL
			}
		}
		if dh.emitIntros {
			if dh.docIntros != "" {
				dd.IntrospectionEndpoint = dh.docIntros
			} else {
				dd.IntrospectionEndpoint = dh.intros.URL
			}
		}
		body, _ := json.Marshal(dd)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(dh.doc.Close)
	if dh.docIssuer == "" {
		dh.docIssuer = dh.doc.URL
	}
	return dh
}

func TestFetchDiscovery_HappyPath(t *testing.T) {
	h := newDiscoveryHarness(t)
	doc, err := FetchDiscovery(context.Background(), h.doc.URL, nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if doc.JWKSURI != h.jwks.server.URL {
		t.Errorf("jwks_uri = %q, want %q", doc.JWKSURI, h.jwks.server.URL)
	}
	if doc.IntrospectionEndpoint != h.intros.URL {
		t.Errorf("introspection_endpoint = %q, want %q", doc.IntrospectionEndpoint, h.intros.URL)
	}
}

func TestFetchDiscovery_Unreachable(t *testing.T) {
	_, err := FetchDiscovery(context.Background(), "http://127.0.0.1:1", nil, 50*time.Millisecond)
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

func TestFetchDiscovery_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()
	_, err := FetchDiscovery(context.Background(), srv.URL, nil, time.Second)
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v", err)
	}
}

// TestNewDiscoveryValidator_DefaultPrefersLocalJWT — happy path
// with default config returns a LocalJWTValidator able to verify
// a signed token.
func TestNewDiscoveryValidator_DefaultPrefersLocalJWT(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.docIssuer = "https://issuer.example"

	v, err := NewDiscoveryValidator(context.Background(), DiscoveryConfig{
		IssuerURL: h.doc.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(*LocalJWTValidator); !ok {
		t.Errorf("expected LocalJWTValidator, got %T", v)
	}

	// Sign a token with the harness key + iss matching the
	// document; validator should accept.
	token := h.jwks.sign(t, jwt.MapClaims{
		"iss":   "https://issuer.example",
		"email": "alice@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	c, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice@example.com" {
		t.Errorf("username = %q", c.Username)
	}
}

// TestNewDiscoveryValidator_PreferIntrospection — flips preference;
// the introspection endpoint is used instead.
func TestNewDiscoveryValidator_PreferIntrospection(t *testing.T) {
	h := newDiscoveryHarness(t)
	v, err := NewDiscoveryValidator(context.Background(), DiscoveryConfig{
		IssuerURL:           h.doc.URL,
		PreferIntrospection: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(*IntrospectionValidator); !ok {
		t.Errorf("expected IntrospectionValidator, got %T", v)
	}
	c, err := v.Validate(context.Background(), "tkn")
	if err != nil {
		t.Fatal(err)
	}
	if c.Username != "alice@example.com" {
		t.Errorf("username = %q", c.Username)
	}
}

// TestNewDiscoveryValidator_FallbackToOther — preferred endpoint
// absent → fall through.
func TestNewDiscoveryValidator_FallbackToOther(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.emitJWKS = false // omit jwks_uri

	v, err := NewDiscoveryValidator(context.Background(), DiscoveryConfig{
		IssuerURL: h.doc.URL, // default prefers JWKS, but it's missing
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(*IntrospectionValidator); !ok {
		t.Errorf("expected IntrospectionValidator fallback, got %T", v)
	}
}

// TestNewDiscoveryValidator_BothMissing → ErrUpstream config error.
func TestNewDiscoveryValidator_BothMissing(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.emitJWKS = false
	h.emitIntros = false
	_, err := NewDiscoveryValidator(context.Background(), DiscoveryConfig{
		IssuerURL: h.doc.URL,
	})
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want ErrUpstream", err)
	}
}

// TestNewDiscoveryValidator_AutoAddIssuer — document's iss is
// added to Issuers automatically so unforged tokens from the
// configured IdP pass without explicit operator config.
func TestNewDiscoveryValidator_AutoAddIssuer(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.docIssuer = "https://expected-issuer.example"

	v, err := NewDiscoveryValidator(context.Background(), DiscoveryConfig{
		IssuerURL: h.doc.URL, // operator did NOT set Issuers explicitly
	})
	if err != nil {
		t.Fatal(err)
	}
	// Token with matching iss → accept.
	good := h.jwks.sign(t, jwt.MapClaims{
		"iss":   "https://expected-issuer.example",
		"email": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Validate(context.Background(), good); err != nil {
		t.Errorf("matching iss rejected: %v", err)
	}
	// Token with attacker iss → reject (because document's iss
	// got auto-added; nothing else passes).
	bad := h.jwks.sign(t, jwt.MapClaims{
		"iss":   "https://attacker.example",
		"email": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Validate(context.Background(), bad); !errors.Is(err, ErrIssuerMismatch) {
		t.Errorf("attacker iss accepted; err=%v", err)
	}
}

// Unused import guard for base64 if test file later imports more.
var _ = base64.RawURLEncoding
