// Package oauth2 validates OAuth 2.0 bearer tokens for the SASL
// OAUTHBEARER mechanism (RFC 7628) and exposes the result as a
// passdb.Passdb so OAuth-authenticated logins flow through the
// regular yarilo-auth pipeline (chain ordering, caching, penalty,
// policy, audit log — all work unchanged).
//
// Three validation modes are supported:
//
//   - LocalJWTValidator — JWT signature verification against a JWKS
//     endpoint. No HTTP call per login (JWKS is cached + auto-
//     refreshed on key rotation).
//   - TokeninfoValidator — Google-style direct endpoint
//     (tokeninfo_url + ?access_token=…) that returns claims JSON.
//   - IntrospectionValidator — RFC 7662 token introspection.
//     Three transport modes: bearer header, query param, form body.
//
// OIDCDiscovery wraps any of the three by auto-resolving endpoints
// from `<issuer>/.well-known/openid-configuration`. Operators that
// configure an issuer URL get JWKS + introspection URLs for free.
//
// Username mapping is config-driven:
//
//   - username_attribute (default "email") — claim name to extract
//   - username_validation_format (default "%{user}") — template
//     applied to the SASL authzid; the extracted claim must equal
//     the templated authzid (case-insensitive when "%Lu" is used)
//
// All validators populate Claims with the raw token claim set so
// the passdb can hand operator-extra claims back to the chain via
// extra_fields (RFC 7662 sub, scope, custom org/role claims, etc.).
package oauth2

import (
	"context"
	"errors"
	"fmt"
)

// Claims holds the validated token's claim set. Mandatory claims
// (Username, Issuer, ExpiresAt) are exposed as struct fields so
// the passdb can short-circuit common checks without dipping into
// the map. Operator-extra claims live in Extra — driver
// configuration decides which Extra keys land on the AuthResponse
// as userdb_* fields.
type Claims struct {
	// Username is the value the SASL authzid is compared against,
	// after applying username_validation_format. Source: the claim
	// named by Config.UsernameAttribute (default "email").
	Username string

	// Issuer (iss) — set when present. Empty for non-JWT tokens
	// returned by an introspection endpoint that omits the field.
	Issuer string

	// Subject (sub) — opaque user identifier. Distinct from
	// Username; some IdPs put a UUID in sub and email in email.
	Subject string

	// ExpiresAt (exp). Zero when the source token format does not
	// carry an expiry (rare; introspection responses usually do).
	ExpiresAt int64

	// IssuedAt (iat). Zero when absent.
	IssuedAt int64

	// Scope is the space-separated scope string the token was
	// issued for. Either a single space-separated string in `scope`
	// (RFC 6749) or `scp` (Microsoft variant).
	Scope string

	// Audience (aud) — empty string or a single value. JWT spec
	// allows an array; we accept either and flatten to the first
	// entry when an array is present.
	Audience string

	// Active is the per-token authoritativeness flag. RFC 7662
	// introspection responses set "active": false to indicate a
	// revoked or expired token; local JWT validators infer it from
	// the signature + exp + nbf checks. Validators MUST NOT return
	// (Claims, nil) when Active is false — they translate the
	// inactive state into ErrTokenInactive.
	Active bool

	// Extra holds every other claim from the token response.
	// Keys preserve the original case from the source. Values are
	// the raw JSON values converted to strings (numbers via %v,
	// bools as "true"/"false", strings verbatim).
	Extra map[string]string
}

// Validator is the abstract token-validation operation. Each
// configured OAuth provider builds one of LocalJWT, Tokeninfo,
// Introspection (optionally wrapped by OIDC discovery) and
// registers it under a name; the OAuth passdb consults its
// configured validator on every OAUTHBEARER login.
//
// Implementations MUST be safe for concurrent use — the passdb
// calls Validate from goroutines serving independent connections.
type Validator interface {
	// Validate examines the supplied bearer token, performs every
	// configured check (signature / issuer / audience / scope /
	// active / exp+grace), and either returns the parsed Claims
	// or an error from this package's error set.
	//
	// The context is the auth request context — Validate honours
	// cancellation (HTTP introspection lookups, JWKS refreshes).
	// Network errors return errors that wrap ErrUpstream so the
	// passdb can map them to ResultTempFail (the chain falls
	// through to the next passdb instead of blocking the user).
	Validate(ctx context.Context, token string) (*Claims, error)
}

// Errors returned by Validator implementations. The passdb maps
// them to chain results:
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

// CheckActive returns ErrInactiveAccount when the configured
// active attribute is present in claims but its value does not
// match the configured active value. Returns nil when no check
// is configured or when the check passes.
//
// Semantics: `active_attribute=enabled active_value=true` means
// the claim `enabled` must equal the string "true" for the login
// to continue. Empty active_attribute disables the check; empty
// active_value with non-empty active_attribute requires the
// attribute to be merely PRESENT.
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
