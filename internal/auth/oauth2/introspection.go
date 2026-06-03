package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IntrospectionMode picks how the validator transports the token
// to the introspection endpoint.
type IntrospectionMode string

const (
	// IntrospectionAuthBearer — POST with empty body and an
	// Authorization: Bearer <token> header.
	IntrospectionAuthBearer IntrospectionMode = "auth"

	// IntrospectionGet — GET <url>?token=<token>. Simplest mode
	// but exposes the token in URLs / access logs.
	IntrospectionGet IntrospectionMode = "get"

	// IntrospectionPostForm — RFC 7662 standard. POST with
	// Content-Type: application/x-www-form-urlencoded and
	// `token=<token>` in the body.
	IntrospectionPostForm IntrospectionMode = "post"
)

// IntrospectionConfig configures an IntrospectionValidator.
type IntrospectionConfig struct {
	// URL is the introspection endpoint. REQUIRED.
	URL string

	// Mode picks the transport. Default IntrospectionPostForm.
	Mode IntrospectionMode

	// ClientID + ClientSecret authenticate the introspection call
	// itself. RFC 7662 endpoints typically require this so an
	// attacker cannot probe arbitrary tokens. When empty, no
	// HTTP basic auth is sent.
	ClientID     string
	ClientSecret string

	// UsernameAttribute is the response claim that resolves to the
	// mail user. Default "email" (Workspace/M365). RFC 7662
	// defines "username"; many real-world deployments override.
	UsernameAttribute string

	// Issuers / Audience / RequiredScopes: same semantics as
	// LocalJWTConfig — operator-side constraints applied to the
	// claims the introspection endpoint returns.
	Issuers        []string
	Audience       string
	RequiredScopes []string

	// ExpireGrace allows tokens whose `exp` lies within this many
	// seconds of now to still pass. Default 60s. Introspection
	// endpoints typically return absolute `exp` claims.
	ExpireGrace time.Duration

	// HTTPTimeout caps the introspection round-trip. Default 5s.
	HTTPTimeout time.Duration

	// HTTPClient is the underlying transport. nil → an
	// http.Client with Timeout = HTTPTimeout. Override for
	// tests / metrics instrumentation.
	HTTPClient *http.Client
}

// IntrospectionValidator validates a bearer token by calling the
// configured RFC 7662 introspection endpoint and parsing the
// response. Behavior:
//
//   - "active": false                  → ErrTokenInactive
//   - HTTP non-2xx                     → ErrUpstream
//   - JSON parse failure               → ErrUpstream
//   - exp present and elapsed (-grace) → ErrTokenExpired
//   - claims satisfy issuer/audience/scope → success
type IntrospectionValidator struct {
	cfg IntrospectionConfig
	hc  *http.Client
}

// NewIntrospectionValidator constructs the validator and applies
// defaults. URL="" or Mode="" rejected at construction so a typo
// fails loudly at startup.
func NewIntrospectionValidator(cfg IntrospectionConfig) (*IntrospectionValidator, error) {
	if cfg.URL == "" {
		return nil, errors.New("oauth2: Introspection requires URL")
	}
	if cfg.Mode == "" {
		cfg.Mode = IntrospectionPostForm
	}
	switch cfg.Mode {
	case IntrospectionAuthBearer, IntrospectionGet, IntrospectionPostForm:
		// ok
	default:
		return nil, fmt.Errorf("oauth2: unsupported introspection mode %q", cfg.Mode)
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
	return &IntrospectionValidator{cfg: cfg, hc: hc}, nil
}

// Validate POSTs / GETs the token per Mode, parses the response,
// and applies the configured claim checks.
func (v *IntrospectionValidator) Validate(ctx context.Context, token string) (*Claims, error) {
	req, err := v.buildRequest(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

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

	// RFC 7662 §2.2: a value of false for "active" means the
	// token is invalid; we treat absent "active" as
	// authoritatively absent (assume false) so a misbehaving IdP
	// cannot accidentally validate tokens.
	if !claims.Active {
		return nil, ErrTokenInactive
	}

	if claims.ExpiresAt > 0 {
		if time.Now().Add(-v.cfg.ExpireGrace).Unix() > claims.ExpiresAt {
			return nil, fmt.Errorf("%w: exp=%d", ErrTokenExpired, claims.ExpiresAt)
		}
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

// buildRequest assembles the HTTP request per the configured mode.
func (v *IntrospectionValidator) buildRequest(ctx context.Context, token string) (*http.Request, error) {
	switch v.cfg.Mode {
	case IntrospectionAuthBearer:
		req, err := http.NewRequestWithContext(ctx, "POST", v.cfg.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if v.cfg.ClientID != "" {
			req.SetBasicAuth(v.cfg.ClientID, v.cfg.ClientSecret)
		}
		return req, nil
	case IntrospectionGet:
		u, err := url.Parse(v.cfg.URL)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("token", token)
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if v.cfg.ClientID != "" {
			req.SetBasicAuth(v.cfg.ClientID, v.cfg.ClientSecret)
		}
		return req, nil
	case IntrospectionPostForm:
		form := url.Values{}
		form.Set("token", token)
		req, err := http.NewRequestWithContext(ctx, "POST", v.cfg.URL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if v.cfg.ClientID != "" {
			req.SetBasicAuth(v.cfg.ClientID, v.cfg.ClientSecret)
		}
		return req, nil
	}
	return nil, fmt.Errorf("oauth2: introspection mode %q unimplemented", v.cfg.Mode)
}

// introspectionToClaims projects the RFC 7662 response into our
// Claims struct.
func introspectionToClaims(m map[string]interface{}, usernameAttribute string) *Claims {
	c := &Claims{Extra: make(map[string]string, len(m))}
	for k, v := range m {
		sval := valueToString(v)
		switch k {
		case "iss":
			c.Issuer = sval
		case "sub":
			c.Subject = sval
		case "exp":
			c.ExpiresAt = toUnix(v)
		case "iat":
			c.IssuedAt = toUnix(v)
		case "scope", "scp":
			c.Scope = sval
		case "aud":
			if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
				c.Audience = valueToString(arr[0])
			} else {
				c.Audience = sval
			}
		case "active":
			if b, ok := v.(bool); ok {
				c.Active = b
			}
		default:
			c.Extra[k] = sval
		}
	}
	if u, ok := m[usernameAttribute]; ok {
		c.Username = valueToString(u)
	}
	return c
}
