// Package oauth2 validates OAuth 2.0 bearer tokens for the SASL OAUTHBEARER
// mechanism (RFC 7628) and exposes the result as a passdb.Passdb, so OAuth
// logins flow through the regular yarilo-auth pipeline unchanged.
//
// Three validation modes:
//
//   - LocalJWTValidator — JWT signature check against a cached, auto-refreshed
//     JWKS; no HTTP call per login.
//   - TokeninfoValidator — direct endpoint (tokeninfo_url + ?access_token=…)
//     returning claims JSON.
//   - IntrospectionValidator — RFC 7662 introspection (bearer header, query
//     param, or form body transport).
//
// OIDCDiscovery wraps any of the three, resolving endpoints from
// `<issuer>/.well-known/openid-configuration`.
//
// Username mapping is config-driven:
//
//   - username_attribute (default "email") — claim name to extract
//   - username_validation_format (default "%{user}") — template applied to the
//     SASL authzid; the extracted claim must equal it (case-insensitive
//     with "%Lu")
//
// Validators populate Claims with the raw claim set so the passdb can hand
// operator-extra claims back to the chain via extra_fields.
package oauth2

import (
	"context"
	"errors"
	"fmt"
)

// Claims holds the validated token's claim set. Common claims are struct
// fields; every other claim lives in Extra, from which driver configuration
// decides which keys land on the AuthResponse as userdb_* fields.
type Claims struct {
	// Username is compared against the SASL authzid after applying
	// username_validation_format. Source: Config.UsernameAttribute.
	Username string

	// Issuer (iss) — empty when the source token omits it.
	Issuer string

	// Subject (sub) — opaque user identifier, distinct from Username.
	Subject string

	// ExpiresAt (exp) — zero when the token carries no expiry.
	ExpiresAt int64

	// IssuedAt (iat) — zero when absent.
	IssuedAt int64

	// Scope — space-separated scope string, from `scope` (RFC 6749) or
	// `scp` (Microsoft variant).
	Scope string

	// Audience (aud) — a single value; an array is flattened to its first entry.
	Audience string

	// Active is the per-token authoritativeness flag: RFC 7662 sets
	// "active": false for a revoked/expired token; JWT validators infer it
	// from signature + exp + nbf. Validators MUST NOT return (Claims, nil)
	// when Active is false — they return ErrTokenInactive.
	Active bool

	// Extra holds every other claim, keys in their source case, values as
	// strings (numbers via %v, bools as "true"/"false", strings verbatim).
	Extra map[string]string
}

// Validator is the abstract token-validation operation. Each configured OAuth
// provider builds one (optionally wrapped by OIDC discovery); the OAuth passdb
// consults it on every OAUTHBEARER login.
//
// Implementations MUST be safe for concurrent use.
type Validator interface {
	// Validate runs every configured check (signature / issuer / audience /
	// scope / active / exp+grace) and returns the parsed Claims or an error
	// from this package's error set.
	//
	// Validate honours ctx cancellation (introspection lookups, JWKS
	// refreshes). Network errors wrap ErrUpstream so the passdb maps them
	// to ResultTempFail and the chain falls through instead of blocking.
	Validate(ctx context.Context, token string) (*Claims, error)
}

// Errors returned by Validator implementations. The passdb maps them to
// chain results:
//
//   - ErrTokenInvalid       → ResultFail (next passdb gets a try)
//   - ErrTokenExpired       → ResultFail
//   - ErrTokenInactive      → ResultFail
//   - ErrIssuerMismatch     → ResultFail
//   - ErrAudienceMismatch   → ResultFail
//   - ErrScopeMissing       → ResultFail
//   - ErrUsernameMismatch   → ResultFail
//   - ErrUpstream (wrapped) → ResultTempFail
//
// Callers should use errors.Is, not equality comparisons.
var (
	ErrTokenInvalid     = errors.New("oauth2: token signature invalid")
	ErrTokenExpired     = errors.New("oauth2: token expired")
	ErrTokenInactive    = errors.New("oauth2: token not active (revoked or not yet valid)")
	ErrIssuerMismatch   = errors.New("oauth2: token issuer not in allowed list")
	ErrAudienceMismatch = errors.New("oauth2: token audience does not match")
	ErrScopeMissing     = errors.New("oauth2: required scope missing from token")
	ErrUsernameMissing  = errors.New("oauth2: username claim missing from token")
	ErrUsernameMismatch = errors.New("oauth2: token username does not match SASL authzid")
	ErrInactiveAccount  = errors.New("oauth2: account marked inactive by active_attribute")
	ErrUpstream         = errors.New("oauth2: upstream validation endpoint unreachable")
)

// CheckActive returns ErrInactiveAccount when the configured active attribute
// is present but its value does not match activeValue; nil when no check is
// configured or it passes.
//
// `active_attribute=enabled active_value=true` requires claim `enabled` to
// equal "true". Empty active_attribute disables the check; empty active_value
// requires the attribute merely to be present.
func CheckActive(c *Claims, activeAttribute, activeValue string) error {
	if activeAttribute == "" {
		return nil
	}
	got, ok := c.Extra[activeAttribute]
	if !ok {
		return fmt.Errorf("%w: claim %q absent", ErrInactiveAccount, activeAttribute)
	}
	if activeValue == "" {
		return nil
	}
	if got != activeValue {
		return fmt.Errorf("%w: %s=%q (want %q)", ErrInactiveAccount, activeAttribute, got, activeValue)
	}
	return nil
}
