package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// TokeninfoConfig configures a TokeninfoValidator. The tokeninfo
// endpoint is a Google-specific OAuth 2.0 server interface
// (https://oauth2.googleapis.com/tokeninfo) that returns claims
// JSON for a supplied access token. Unlike RFC 7662 introspection
// it has no spec — every IdP that exposes a tokeninfo endpoint
// has a slightly different response shape, but the call shape
// (GET ?access_token=...) is consistent enough to share code.
type TokeninfoConfig struct {
	// URL is the tokeninfo endpoint. REQUIRED. Token is appended
	// as ?access_token=<token>. Operators set this to e.g.
	// `https://oauth2.googleapis.com/tokeninfo`.
	URL string

	// UsernameAttribute is the response claim that resolves to
	// the mail user. Default "email" (matches Google's tokeninfo
	// payload).
	UsernameAttribute string

	// Issuers / Audience / RequiredScopes / ExpireGrace mirror
	// LocalJWTConfig — operator constraints applied to the
	// tokeninfo response claims.
	Issuers        []string
	Audience       string
	RequiredScopes []string
	ExpireGrace    time.Duration

	// HTTPTimeout caps the round-trip. Default 5s.
	HTTPTimeout time.Duration

	// HTTPClient overrides the transport. nil → http.Client with
	// Timeout = HTTPTimeout.
	HTTPClient *http.Client
}

// TokeninfoValidator GETs the tokeninfo endpoint with the token
// in the query string, then runs the same claim-extraction
// pipeline as IntrospectionValidator (the response shapes are
// near-identical for our purposes).
type TokeninfoValidator struct {
	cfg TokeninfoConfig
	hc  *http.Client
}

// NewTokeninfoValidator constructs the validator and applies
// defaults. Empty URL rejected.
func NewTokeninfoValidator(cfg TokeninfoConfig) (*TokeninfoValidator, error) {
	if cfg.URL == "" {
		return nil, errors.New("oauth2: Tokeninfo requires URL")
	}
	if cfg.UsernameAttribute == "" {
		cfg.UsernameAttribute = "email"
	}
	if cfg.ExpireGrace == 0 {
		cfg.ExpireGrace = 60 * time.Second
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	return &TokeninfoValidator{cfg: cfg, hc: hc}, nil
}

// Validate calls the tokeninfo endpoint and applies the configured
// claim constraints. A 2xx response with a parseable JSON body is
// treated as "the token is currently valid"; tokeninfo endpoints
// return 4xx for revoked / unknown tokens, which the validator
// surfaces as ErrTokenInactive.
func (v *TokeninfoValidator) Validate(ctx context.Context, token string) (*Claims, error) {
	u, err := url.Parse(v.cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: url parse: %v", ErrUpstream, err)
	}
	q := u.Query()
	q.Set("access_token", token)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, ErrTokenInactive
	}
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%w: HTTP %d", ErrUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUpstream, err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: parse response: %v", ErrUpstream, err)
	}

	claims := introspectionToClaims(raw, v.cfg.UsernameAttribute)
	// tokeninfo endpoints don't set "active"; a 2xx response IS
	// the active signal.
	claims.Active = true

	if claims.ExpiresAt > 0 && time.Now().Add(-v.cfg.ExpireGrace).Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("%w: exp=%d", ErrTokenExpired, claims.ExpiresAt)
	}
	if len(v.cfg.Issuers) > 0 && !containsString(v.cfg.Issuers, claims.Issuer) {
		return nil, fmt.Errorf("%w: iss=%q allowed=%v", ErrIssuerMismatch, claims.Issuer, v.cfg.Issuers)
	}
	if v.cfg.Audience != "" && claims.Audience != v.cfg.Audience {
		return nil, fmt.Errorf("%w: aud=%q want=%q", ErrAudienceMismatch, claims.Audience, v.cfg.Audience)
	}
	if len(v.cfg.RequiredScopes) > 0 && !scopeContainsAll(claims.Scope, v.cfg.RequiredScopes) {
		return nil, fmt.Errorf("%w: scope=%q need=%v", ErrScopeMissing, claims.Scope, v.cfg.RequiredScopes)
	}
	return claims, nil
}
