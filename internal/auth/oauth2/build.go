package oauth2

import (
	"context"
	"fmt"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/config"
)

// BuildPassdbs constructs one Passdb per configured OAuth2Entry.
// Returns the list in entry order so the configuration's intent
// (which provider has chain priority) is preserved.
//
// Validation failures at construction (missing required URL,
// unknown mode, JWKS fetch failure) propagate as errors so the
// operator notices misconfiguration at startup rather than at
// first login.
func BuildPassdbs(ctx context.Context, entries []config.OAuth2Entry) ([]protocol.Passdb, error) {
	out := make([]protocol.Passdb, 0, len(entries))
	for i, e := range entries {
		pdb, err := buildOne(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("oauth2 entry #%d (%s): %w", i, e.Mode, err)
		}
		out = append(out, pdb)
	}
	return out, nil
}

func buildOne(ctx context.Context, e config.OAuth2Entry) (protocol.Passdb, error) {
	timeout := time.Duration(e.HTTPTimeoutMs) * time.Millisecond
	grace := time.Duration(e.TokenExpireGraceSeconds) * time.Second

	var validator Validator
	var err error
	switch e.Mode {
	case config.OAuth2ModeLocalJWT:
		validator, err = NewLocalJWTValidator(ctx, LocalJWTConfig{
			JWKSURL:           e.JWKSURL,
			Issuers:           e.Issuers,
			Audience:          e.Audience,
			RequiredScopes:    e.Scopes,
			UsernameAttribute: e.UsernameAttribute,
			ExpireGrace:       grace,
			HTTPTimeout:       timeout,
		})
	case config.OAuth2ModeIntrospection:
		validator, err = NewIntrospectionValidator(IntrospectionConfig{
			URL:               e.IntrospectionURL,
			Mode:              introMode(e.IntrospectionMode),
			ClientID:          e.ClientID,
			ClientSecret:      e.ClientSecret,
			Issuers:           e.Issuers,
			Audience:          e.Audience,
			RequiredScopes:    e.Scopes,
			UsernameAttribute: e.UsernameAttribute,
			ExpireGrace:       grace,
			HTTPTimeout:       timeout,
		})
	case config.OAuth2ModeTokeninfo:
		validator, err = NewTokeninfoValidator(TokeninfoConfig{
			URL:               e.TokeninfoURL,
			Issuers:           e.Issuers,
			Audience:          e.Audience,
			RequiredScopes:    e.Scopes,
			UsernameAttribute: e.UsernameAttribute,
			ExpireGrace:       grace,
			HTTPTimeout:       timeout,
		})
	case config.OAuth2ModeDiscovery:
		validator, err = NewDiscoveryValidator(ctx, DiscoveryConfig{
			IssuerURL:           e.IssuerURL,
			PreferIntrospection: e.PreferIntrospection,
			IntrospectionMode:   introMode(e.IntrospectionMode),
			ClientID:            e.ClientID,
			ClientSecret:        e.ClientSecret,
			Issuers:             e.Issuers,
			Audience:            e.Audience,
			RequiredScopes:      e.Scopes,
			UsernameAttribute:   e.UsernameAttribute,
			ExpireGrace:         grace,
			HTTPTimeout:         timeout,
		})
	default:
		return nil, fmt.Errorf("oauth2: unknown mode %q (valid: local | introspection | tokeninfo | discovery)", e.Mode)
	}
	if err != nil {
		return nil, err
	}
	return NewPassdb(PassdbConfig{
		Validator:        validator,
		UsernameTemplate: e.UsernameValidationFormat,
		ActiveAttribute:  e.ActiveAttribute,
		ActiveValue:      e.ActiveValue,
		ExtraFields:      e.ExtraFields,
	})
}

func introMode(raw string) IntrospectionMode {
	switch raw {
	case "auth":
		return IntrospectionAuthBearer
	case "get":
		return IntrospectionGet
	case "", "post":
		return IntrospectionPostForm
	default:
		// Unknown values caught by NewIntrospectionValidator.
		return IntrospectionMode(raw)
	}
}
