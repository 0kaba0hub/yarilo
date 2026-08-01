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
// (OpenID Connect Discovery 1.0 §3) needed to build a validator.
type DiscoveryDocument struct {
	Issuer                string   `json:"issuer"`
	JWKSURI               string   `json:"jwks_uri"`
	IntrospectionEndpoint string   `json:"introspection_endpoint"`
	TokeninfoEndpoint     string   `json:"tokeninfo_endpoint,omitempty"`
	ScopesSupported       []string `json:"scopes_supported,omitempty"`
}

// FetchDiscovery fetches `<issuer>/.well-known/openid-configuration`.
// Errors wrap ErrUpstream.
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

// DiscoveryConfig configures how a validator is built from an OIDC
// discovery document.
type DiscoveryConfig struct {
	// IssuerURL is the OAuth provider issuer URL. Required.
	IssuerURL string

	// PreferIntrospection picks the introspection endpoint over local JWKS
	// when both are advertised. Default false: local JWT validation avoids a
	// per-login HTTP call.
	PreferIntrospection bool

	// IntrospectionMode is the introspection transport. Default
	// IntrospectionPostForm.
	IntrospectionMode IntrospectionMode

	// ClientID / ClientSecret for the introspection endpoint (ignored for
	// local JWT).
	ClientID     string
	ClientSecret string

	// Shared constraints for whichever validator ends up active. The
	// document's issuer is auto-added to Issuers.
	Issuers           []string
	Audience          string
	RequiredScopes    []string
	UsernameAttribute string
	ExpireGrace       time.Duration
	HTTPTimeout       time.Duration
}

// NewDiscoveryValidator fetches the OIDC document and builds a
// LocalJWTValidator or IntrospectionValidator, preferring jwks_uri unless
// PreferIntrospection is set. A document with neither endpoint is ErrUpstream.
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
	// Auto-add the document's iss for the local-JWT path only: introspection
	// responses (RFC 7662) often omit iss, so requiring it there would
	// reject every token.
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
	// First preference unavailable; try the second.
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
