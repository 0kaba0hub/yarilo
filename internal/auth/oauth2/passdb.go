package oauth2

import (
	"context"
	"errors"
	"fmt"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// PassdbConfig configures how a Validator is exposed as a
// protocol.Passdb. The validator handles all token verification;
// the passdb wraps it with username comparison, active-attribute
// check, and field projection.
type PassdbConfig struct {
	// Validator is the configured token-validator. REQUIRED.
	Validator Validator

	// UsernameTemplate is the substitution format applied to the
	// SASL authzid before comparison against the token's username
	// claim. Default "%{user}" (identity).
	UsernameTemplate string

	// ActiveAttribute / ActiveValue — optional account-active
	// check on a claim NOT named by the validator's
	// UsernameAttribute. When ActiveAttribute is set, the claim
	// must be present (and equal ActiveValue if non-empty).
	ActiveAttribute string
	ActiveValue     string

	// ExtraFields selects which claims to surface back to the
	// auth pipeline as userdb_* fields. Empty = none. The list
	// contains the claim names; their string values land under
	// `userdb_<name>` in the result bag.
	ExtraFields []string

	// LookupTimeout caps the per-request Validate call. Zero
	// inherits the validator's own timeouts.
	LookupTimeout int
}

// Passdb adapts a Validator to the protocol.Passdb surface so an
// OAUTHBEARER login flows through the same chain machinery as
// SQL passdb logins (cache, penalty, policy, audit log, ordering
// fall-through to the next passdb).
//
// Authentication contract: the SASL OAUTHBEARER handler stuffs
// the bearer token into req.Password and the GS2 authzid into
// req.Username. The passdb extracts both and:
//
//  1. Calls Validator.Validate(token) — signature / iss / aud /
//     scope / exp checks.
//  2. CompareUsername(token-claim, req.Username, template) —
//     enforces that the SASL-claimed user matches the token's
//     username claim.
//  3. CheckActive(claims, ActiveAttribute, ActiveValue) — optional
//     account-active claim check.
//  4. Writes user, userdb_* extra-fields onto req.Fields and
//     returns ResultOK.
//
// Errors map to chain results:
//
//   - ErrUpstream    → ResultTempFail (let the chain retry / fall to next passdb)
//   - everything else → ResultNext (token rejected; chain may try a different passdb)
//
// Note we return ResultNext (not ResultFail) on validation
// rejection so a deployment with `oauth2 → sql` chain order can
// still try SQL when the token does not match (e.g. plain-PLAIN
// from a non-OAuth client routed via the same SASL endpoint).
type Passdb struct {
	cfg PassdbConfig
}

// NewPassdb constructs the passdb. Validator MUST be non-nil.
func NewPassdb(cfg PassdbConfig) (*Passdb, error) {
	if cfg.Validator == nil {
		return nil, errors.New("oauth2/passdb: Validator is required")
	}
	if cfg.UsernameTemplate == "" {
		cfg.UsernameTemplate = "%{user}"
	}
	return &Passdb{cfg: cfg}, nil
}

// Authenticate satisfies protocol.Passdb.
func (p *Passdb) Authenticate(req *protocol.Request) (protocol.Result, error) {
	token := req.Password
	if token == "" {
		// No bearer token to validate — let the next passdb try.
		return protocol.ResultNext, nil
	}

	claims, err := p.cfg.Validator.Validate(context.Background(), token)
	if err != nil {
		// Upstream errors are transient; fall through to ResultTempFail
		// so the operator's failure-delay / penalty kick in correctly.
		if errors.Is(err, ErrUpstream) {
			return protocol.ResultTempFail, err
		}
		// Token validation failure — let the chain try another
		// passdb. Logged at slog.Debug by the chain wrapper.
		return protocol.ResultNext, nil
	}

	// Username comparison — SASL authzid vs token claim.
	resolvedUser, err := CompareUsername(claims.Username, req.Username, p.cfg.UsernameTemplate)
	if err != nil {
		return protocol.ResultNext, nil
	}
	if claims.Username == "" {
		// Token had no username claim at all — clear miss.
		return protocol.ResultNext, fmt.Errorf("%w: validator returned no Username", ErrUsernameMissing)
	}

	// Active-account claim check.
	if err := CheckActive(claims, p.cfg.ActiveAttribute, p.cfg.ActiveValue); err != nil {
		return protocol.ResultNext, nil
	}

	// Project claims onto the auth-request fields.
	if req.Fields == nil {
		req.Fields = protocol.NewFields()
	}
	req.Fields.Set("user", resolvedUser)
	for _, claim := range p.cfg.ExtraFields {
		if v, ok := claims.Extra[claim]; ok && v != "" {
			req.Fields.Set("userdb_"+claim, v)
		}
	}
	return protocol.ResultOK, nil
}
