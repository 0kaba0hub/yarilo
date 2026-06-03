package oauth2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// LocalJWTConfig configures a LocalJWTValidator. The JWKS endpoint
// publishes the IdP's signing keys; the validator caches them with
// auto-refresh on key rotation.
type LocalJWTConfig struct {
	// JWKSURL is the JSON Web Key Set endpoint (typically
	// `<issuer>/.well-known/jwks.json` or surfaced by OIDC
	// discovery). REQUIRED.
	JWKSURL string

	// Issuers lists allowed `iss` claim values. Empty = no check
	// (any signed token from a key in the JWKS is accepted —
	// useful for single-tenant IdPs where the JWKS is the only
	// trust root).
	Issuers []string

	// Audience constrains the `aud` claim. Empty = no check.
	// Single string — JWT spec allows an array of audiences, the
	// validator accepts either provided one entry matches.
	Audience string

	// RequiredScopes are scope strings every token MUST carry
	// (intersection check). Empty = no check.
	RequiredScopes []string

	// UsernameAttribute is the claim name whose value resolves to
	// the mail user identity. Default "email".
	UsernameAttribute string

	// ExpireGrace allows tokens whose `exp` lies within this many
	// seconds of now to still pass. Clock-skew tolerance. Default
	// 60 seconds.
	ExpireGrace time.Duration

	// RefreshInterval is how often the JWKS cache silently
	// refreshes in the background. Default 1 hour.
	RefreshInterval time.Duration

	// HTTPTimeout caps the JWKS fetch round-trip. Default 5s.
	HTTPTimeout time.Duration
}

// LocalJWTValidator verifies JWT bearer tokens locally against a
// cached JWKS. No HTTP call per login — JWKS refresh happens on a
// timer or after a signature-verify miss (unknown kid).
type LocalJWTValidator struct {
	cfg LocalJWTConfig
	kf  keyfunc.Keyfunc
}

// NewLocalJWTValidator constructs the validator and prefetches the
// JWKS so the first login does not pay the round-trip latency. A
// JWKS-fetch failure at construction time returns an error so the
// operator notices misconfiguration immediately.
func NewLocalJWTValidator(ctx context.Context, cfg LocalJWTConfig) (*LocalJWTValidator, error) {
	if cfg.JWKSURL == "" {
		return nil, fmt.Errorf("oauth2: LocalJWT requires JWKSURL")
	}
	if cfg.UsernameAttribute == "" {
		cfg.UsernameAttribute = "email"
	}
	if cfg.ExpireGrace == 0 {
		cfg.ExpireGrace = 60 * time.Second
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = time.Hour
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
	if err != nil {
		return nil, fmt.Errorf("oauth2: JWKS fetch %s: %w", cfg.JWKSURL, errors.Join(ErrUpstream, err))
	}
	return &LocalJWTValidator{cfg: cfg, kf: kf}, nil
}

// Validate parses + verifies the token signature, then checks
// iss / aud / scope / exp+grace / active claims.
func (v *LocalJWTValidator) Validate(_ context.Context, token string) (*Claims, error) {
	tok, err := jwt.Parse(token, v.kf.Keyfunc,
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA"}),
		jwt.WithLeeway(v.cfg.ExpireGrace),
	)
	if err != nil {
		// jwt.Parse returns wrapped sentinels we map to our set.
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, fmt.Errorf("%w: %v", ErrTokenExpired, err)
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return nil, fmt.Errorf("%w: %v", ErrTokenInactive, err)
		default:
			return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
		}
	}

	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims type", ErrTokenInvalid)
	}

	claims := jwtMapToClaims(mc, v.cfg.UsernameAttribute)
	claims.Active = true // signed + non-expired ⇒ active

	if len(v.cfg.Issuers) > 0 && !containsString(v.cfg.Issuers, claims.Issuer) {
		return nil, fmt.Errorf("%w: iss=%q allowed=%v", ErrIssuerMismatch, claims.Issuer, v.cfg.Issuers)
	}
	if v.cfg.Audience != "" {
		if !audienceContains(mc, v.cfg.Audience) {
			return nil, fmt.Errorf("%w: aud=%v want=%q", ErrAudienceMismatch, mc["aud"], v.cfg.Audience)
		}
	}
	if len(v.cfg.RequiredScopes) > 0 {
		if !scopeContainsAll(claims.Scope, v.cfg.RequiredScopes) {
			return nil, fmt.Errorf("%w: scope=%q need=%v", ErrScopeMissing, claims.Scope, v.cfg.RequiredScopes)
		}
	}
	return claims, nil
}

// jwtMapToClaims projects a jwt.MapClaims into our Claims struct.
// Unknown keys land in Extra so the passdb can use any operator-
// specific claim (org_id, role, mailbox_quota, …) downstream.
func jwtMapToClaims(m jwt.MapClaims, usernameAttribute string) *Claims {
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
			// aud may be string or []string; record the first value.
			if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
				c.Audience = valueToString(arr[0])
			} else {
				c.Audience = sval
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

// valueToString flattens a JSON value into a string for storage in
// Extra. Numbers become decimal text; bools become "true" / "false";
// strings pass through verbatim; arrays / objects are JSON-ish
// fallback via fmt %v (operators with structured claims should
// pre-flatten in the IdP).
func valueToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		// JSON numbers decode as float64 in encoding/json. Keep
		// integer formatting when the value is integral so a
		// `uid=1000` claim does not surface as "1000.000000".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// toUnix converts an `exp`/`iat` claim value to a Unix timestamp.
// Numbers (the JSON encoding) become int64 directly; absent or
// malformed claims become 0.
func toUnix(v interface{}) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// audienceContains returns true when the JWT's `aud` claim (a
// string or string array) contains the supplied value.
func audienceContains(m jwt.MapClaims, want string) bool {
	switch a := m["aud"].(type) {
	case string:
		return a == want
	case []interface{}:
		for _, v := range a {
			if s, ok := v.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// scopeContainsAll checks the space-separated `scope` claim
// against a list of required scopes. Returns true only when every
// required scope is present.
func scopeContainsAll(scope string, required []string) bool {
	if scope == "" {
		return false
	}
	got := strings.Fields(scope)
	gset := make(map[string]struct{}, len(got))
	for _, s := range got {
		gset[s] = struct{}{}
	}
	for _, want := range required {
		if _, ok := gset[want]; !ok {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
