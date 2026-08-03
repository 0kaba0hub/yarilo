package jmap

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/oauth2"
)

// errUnauthorized is returned for every rejected credential. The cause is
// logged but never sent: distinguishing "no such user" from "wrong password"
// over an unauthenticated endpoint enumerates accounts.
var errUnauthorized = errors.New("jmap: unauthorized")

// PasswordAuthenticator verifies a username and password. Satisfied by the
// yarilo-auth client, which runs the configured passdb chain.
type PasswordAuthenticator interface {
	Authenticate(username, password, service, remoteIP, sessionID string) (string, error)
}

// authenticate resolves the request's credentials to a username. Bearer is
// tried first: a client that sent one meant it, and falling back to Basic on a
// rejected token would mask token problems as password problems.
func (s *Server) authenticate(r *http.Request) (string, error) {
	scheme, cred, ok := parseAuthorization(r.Header.Get("Authorization"))
	if !ok {
		return "", errUnauthorized
	}
	switch strings.ToLower(scheme) {
	case "bearer":
		return s.authenticateBearer(r.Context(), cred)
	case "basic":
		return s.authenticateBasic(r, cred)
	}
	return "", errUnauthorized
}

func (s *Server) authenticateBearer(ctx context.Context, token string) (string, error) {
	if s.opts.TokenValidator == nil {
		return "", errUnauthorized
	}
	claims, err := s.opts.TokenValidator.Validate(ctx, token)
	if err != nil {
		s.logAuthFailure("bearer", "", err)
		return "", errUnauthorized
	}
	if claims == nil || claims.Username == "" {
		return "", errUnauthorized
	}
	return claims.Username, nil
}

func (s *Server) authenticateBasic(r *http.Request, cred string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return "", errUnauthorized
	}
	username, password, found := strings.Cut(string(raw), ":")
	if !found || username == "" {
		return "", errUnauthorized
	}
	if s.opts.Auth == nil {
		return "", errUnauthorized
	}
	// Plaintext credentials over a cleartext connection are refused outright
	// rather than accepted and logged, matching the other protocols.
	if s.opts.DisablePlainAuth && r.TLS == nil {
		return "", errUnauthorized
	}
	resolved, err := s.opts.Auth.Authenticate(username, password, "jmap", remoteIP(r), "")
	if err != nil {
		s.logAuthFailure("basic", username, err)
		return "", errUnauthorized
	}
	if resolved == "" {
		resolved = username
	}
	return resolved, nil
}

// parseAuthorization splits an Authorization header into scheme and credential.
func parseAuthorization(h string) (scheme, cred string, ok bool) {
	scheme, cred, ok = strings.Cut(strings.TrimSpace(h), " ")
	if !ok {
		return "", "", false
	}
	cred = strings.TrimSpace(cred)
	return scheme, cred, cred != ""
}

// remoteIP is the peer address without the port, for the passdb's allow_nets
// check. Proxy headers are deliberately ignored: trusting them here would let a
// client choose its own source address.
func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// oauth2Validator narrows the oauth2 package to what this server uses, so a
// test can supply its own.
type oauth2Validator = oauth2.Validator
