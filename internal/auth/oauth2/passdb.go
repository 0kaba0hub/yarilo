package oauth2

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// PassdbConfig configures how a Validator is exposed as a
// protocol.Passdb. The validator verifies the token; the passdb wraps it
// with username comparison, active-attribute check, and field
// projection.
type PassdbConfig struct {
	// Validator is the configured token-validator. Required.
	Validator Validator

	// UsernameTemplate is applied to the SASL authzid before comparison
	// against the token's username claim. Default "%{user}" (identity).
	UsernameTemplate string

	// ActiveAttribute / ActiveValue — optional account-active check on a
	// claim other than the validator's UsernameAttribute. When set, the
	// claim must be present (and equal ActiveValue if non-empty).
	ActiveAttribute string
	ActiveValue     string

	// ExtraFields names the claims surfaced back to the auth pipeline as
	// `userdb_<name>` fields. Empty = none.
	ExtraFields []string

	// LookupTimeout caps the per-request Validate call. Zero inherits the
	// validator's own timeouts.
	LookupTimeout int
}

// Passdb adapts a Validator to the protocol.Passdb surface so an
// OAUTHBEARER login flows through the same chain machinery as SQL passdb
// logins (cache, penalty, policy, audit log, fall-through).
//
// Contract: the SASL OAUTHBEARER handler puts the bearer token in
// req.Password and the GS2 authzid in req.Username. The passdb then:
//
//  1. Validator.Validate(token) — signature / iss / aud / scope / exp.
//  2. CompareUsername(claim, req.Username, template) — the SASL-claimed
//     user must match the token's username claim.
//  3. CheckActive(claims, ActiveAttribute, ActiveValue) — optional.
//  4. Writes user + userdb_* fields onto req.Fields, returns ResultOK.
//
// Error mapping:
//
//   - ErrUpstream     → ResultTempFail (chain may retry / fall through)
//   - everything else → ResultNext (token rejected; chain may try another)
//
// Rejection returns ResultNext (not ResultFail) so an `oauth2 → sql`
// chain still tries SQL when the token does not match (e.g. plain PLAIN
// from a non-OAuth client on the same SASL endpoint).
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
		// No bearer token — let the next passdb try.
		return protocol.ResultNext, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	claims, err := p.cfg.Validator.Validate(ctx, token)
	if err != nil {
		// Upstream errors are transient; ResultTempFail so failure-delay
		// / penalty engage correctly.
		if errors.Is(err, ErrUpstream) {
			return protocol.ResultTempFail, err
		}
		// Validation failure — let the chain try another passdb.
		return protocol.ResultNext, nil
	}

	// SASL authzid vs token claim.
	resolvedUser, err := CompareUsername(claims.Username, req.Username, p.cfg.UsernameTemplate)
	if err != nil {
		return protocol.ResultNext, nil
	}
	if claims.Username == "" {
		// No username claim at all — clear miss.
		return protocol.ResultNext, fmt.Errorf("%w: validator returned no Username", ErrUsernameMissing)
	}

	if err := CheckActive(claims, p.cfg.ActiveAttribute, p.cfg.ActiveValue); err != nil {
		return protocol.ResultNext, nil
	}

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

// DriverName satisfies protocol.DriverName for passdb metrics.
func (p *Passdb) DriverName() string { return "oauth2" }
