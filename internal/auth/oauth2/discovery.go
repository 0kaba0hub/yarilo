package oauth2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DiscoveryDocument is the subset of OpenID Provider Configuration
// (per OpenID Connect Discovery 1.0 §3) we consume. The IdP
// publishes the full document at
// `<issuer>/.well-known/openid-configuration`; we read only the
// fields needed to build a validator without operator-side
// endpoint configuration.
type DiscoveryDocument struct {
	Issuer                string   `json:"issuer"`
	JWKSURI               string   `json:"jwks_uri"`
	IntrospectionEndpoint string   `json:"introspection_endpoint"`
	TokeninfoEndpoint     string   `json:"tokeninfo_endpoint,omitempty"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
}

// FetchDiscovery resolves the OpenID configuration document for
// the supplied issuer URL. The lookup URL is
// `<issuer>/.well-known/openid-configuration` (trailing slash on
// issuer is normalised away).
//
// Errors wrap ErrUpstream so callers can distinguish transient
// network failure from value error.
func FetchDiscovery(ctx context.Context, issuerURL string, hc *http.Client, timeout time.Duration) (*DiscoveryDocument, error) {
	if issuerURL == "" {
		return nil, fmt.Errorf("oauth2/discovery: empty issuer URL")
	}
	if hc == nil {
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	u := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%w: discovery %s HTTP %d", ErrUpstream, u, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read discovery body: %v", ErrUpstream, err)
	}

	var doc DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: parse discovery: %v", ErrUpstream, err)
	}
	return &doc, nil
}

// DiscoveryConfig configures how a validator should be built from
// an OIDC discovery document. Mirrors the constructor parameters
// of the underlying validators so operators get the same knobs
// regardless of which transport ends up active.
type DiscoveryConfig struct {
	// IssuerURL is the OAuth provider issuer URL. The OIDC
	// document is fetched from
	// `<IssuerURL>/.well-known/openid-configuration`. REQUIRED.
	IssuerURL string

	// PreferIntrospection picks the introspection endpoint over
	// the local JWKS when both are advertised. Default false:
	// local JWT validation is faster (no per-login HTTP call)
	// and works for any signed token.
	PreferIntrospection bool

	// IntrospectionMode is the transport used when an
	// introspection validator is built. Default
	// IntrospectionPostForm.
	IntrospectionMode IntrospectionMode

	// ClientID / ClientSecret for the introspection endpoint
	// (ignored when local JWT is selected).
	ClientID     string
	ClientSecret string

	// Shared constraints applied to whichever validator ends up
	// active. The discovery document's `issuer` claim is auto-
	// added to Issuers (so an operator who only configures
	// IssuerURL still gets iss-verification for free).
	Issuers           []string
	Audience          string
	RequiredScopes    []string
	UsernameAttribute string
	ExpireGrace       time.Duration
	HTTPTimeout       time.Duration
}

// NewDiscoveryValidator fetches the OIDC document, then constructs
// a LocalJWTValidator or IntrospectionValidator depending on
// what's advertised + PreferIntrospection. Returns a Validator
// that wraps either implementation.
//
// Order of preference:
//
//	PreferIntrospection=true: introspection_endpoint → jwks_uri
//	PreferIntrospection=false (default): jwks_uri → introspection_endpoint
//
// A discovery document missing both endpoints is a configuration
// error and surfaces as ErrUpstream.
func NewDiscoveryValidator(ctx context.Context, cfg DiscoveryConfig) (Validator, error) {
	if cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oauth2/discovery: empty IssuerURL")
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	doc, err := FetchDiscovery(ctx, cfg.IssuerURL, nil, cfg.HTTPTimeout)
	if err != nil {
		return nil, err
	}
	// allowedIssuersForLocalJWT auto-adds the document's iss so an
	// operator who only sets IssuerURL still gets iss verification
	// for free. Done ONLY for the local-JWT path: introspection
	// responses (RFC 7662) often omit the iss claim, so auto-
	// adding the doc's iss there would silently reject every token.
	allowedIssuersForLocalJWT := cfg.Issuers
	if doc.Issuer != "" && !containsString(allowedIssuersForLocalJWT, doc.Issuer) {
		allowedIssuersForLocalJWT = append(allowedIssuersForLocalJWT, doc.Issuer)
	}

	first, second := doc.JWKSURI, doc.IntrospectionEndpoint
	if cfg.PreferIntrospection {
		first, second = doc.IntrospectionEndpoint, doc.JWKSURI
	}

	// Try the first preference; fall back to the second.
	if first == doc.JWKSURI && first != "" {
		return NewLocalJWTValidator(ctx, LocalJWTConfig{
			JWKSURL:           first,
			Issuers:           allowedIssuersForLocalJWT,
			Audience:          cfg.Audience,
			RequiredScopes:    cfg.RequiredScopes,
			UsernameAttribute: cfg.UsernameAttribute,
			ExpireGrace:       cfg.ExpireGrace,
			HTTPTimeout:       cfg.HTTPTimeout,
		})
	}
	if first == doc.IntrospectionEndpoint && first != "" {
		return NewIntrospectionValidator(IntrospectionConfig{
			URL:               first,
			Mode:              cfg.IntrospectionMode,
			ClientID:          cfg.ClientID,
			ClientSecret:      cfg.ClientSecret,
			Issuers:           cfg.Issuers,
			Audience:          cfg.Audience,
			RequiredScopes:    cfg.RequiredScopes,
			UsernameAttribute: cfg.UsernameAttribute,
			ExpireGrace:       cfg.ExpireGrace,
			HTTPTimeout:       cfg.HTTPTimeout,
		})
	}
	// First preference unavailable — fall through to second.
	if second == doc.JWKSURI && second != "" {
		return NewLocalJWTValidator(ctx, LocalJWTConfig{
			JWKSURL:           second,
			Issuers:           allowedIssuersForLocalJWT,
			Audience:          cfg.Audience,
			RequiredScopes:    cfg.RequiredScopes,
			UsernameAttribute: cfg.UsernameAttribute,
			ExpireGrace:       cfg.ExpireGrace,
			HTTPTimeout:       cfg.HTTPTimeout,
		})
	}
	if second == doc.IntrospectionEndpoint && second != "" {
		return NewIntrospectionValidator(IntrospectionConfig{
			URL:               second,
			Mode:              cfg.IntrospectionMode,
			ClientID:          cfg.ClientID,
			ClientSecret:      cfg.ClientSecret,
			Issuers:           cfg.Issuers,
			Audience:          cfg.Audience,
			RequiredScopes:    cfg.RequiredScopes,
			UsernameAttribute: cfg.UsernameAttribute,
			ExpireGrace:       cfg.ExpireGrace,
			HTTPTimeout:       cfg.HTTPTimeout,
		})
	}
	return nil, fmt.Errorf("%w: discovery document advertises neither jwks_uri nor introspection_endpoint", ErrUpstream)
}
