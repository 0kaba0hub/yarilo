// Package scram is the session-layer glue between the SCRAM SASL
// server (in our forked go-sasl) and the per-protocol
// IMAP / POP3 / Submission session handlers in yarilo. It wraps
// sasl.NewScramSha256Server / sasl.NewScramSha256PlusServer with:
//
//  1. A lookup-callback capture so the session learns which user
//     authenticated (the underlying go-sasl factory takes the
//     lookup but does not expose the username back to the caller).
//  2. An onSuccess hook fired the instant the SASL state machine
//     reaches done=true with no error, so the session can run its
//     post-auth setup (open user storage, populate fields) before
//     the protocol framework marks the connection authenticated.
//
// The wrapper preserves the underlying sasl.Server's wire
// semantics — it only inserts the post-success hook.
package scram

import (
	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// Session is the session-layer SASL adapter. Embed-compatible
// with the sasl.Server contract; the protocol framework drives
// it via Next().
type Session struct {
	inner sasl.Server

	// User is set the first time the lookup callback fires.
	// Empty until then.
	User string

	// OnSuccess, when non-nil, runs after the SASL exchange
	// finishes with done=true + err==nil. A non-nil return from
	// OnSuccess is propagated as the SASL error so the protocol
	// layer surfaces it to the client as auth-failed.
	OnSuccess func(user string) error
}

// NewSha256 builds a SCRAM-SHA-256 session adapter wrapping the
// supplied per-user verifier lookup. onSuccess is invoked once
// the exchange completes successfully; nil disables the hook.
func NewSha256(lookup protocol.SCRAMSha256Lookup, onSuccess func(string) error) *Session {
	s := &Session{OnSuccess: onSuccess}
	s.inner = sasl.NewScramSha256Server(func(user string) (*sasl.ScramCredentials, error) {
		s.User = user
		return lookup.LookupSCRAMSha256(user)
	})
	return s
}

// NewSha256Plus is the channel-bound variant. cbData is the TLS
// exporter output the caller computed from the underlying TLS
// conn (RFC 9266); the wrapper passes it into the SASL server
// for the c= verification.
func NewSha256Plus(lookup protocol.SCRAMSha256Lookup, cbData []byte, onSuccess func(string) error) *Session {
	s := &Session{OnSuccess: onSuccess}
	s.inner = sasl.NewScramSha256PlusServer(
		func(user string) (*sasl.ScramCredentials, error) {
			s.User = user
			return lookup.LookupSCRAMSha256(user)
		},
		sasl.ScramServerOptions{ChannelBindingData: cbData},
	)
	return s
}

// Next satisfies sasl.Server. Delegates to the underlying SCRAM
// server and runs OnSuccess after a clean done=true.
func (s *Session) Next(response []byte) (challenge []byte, done bool, err error) {
	challenge, done, err = s.inner.Next(response)
	if done && err == nil && s.OnSuccess != nil {
		if perr := s.OnSuccess(s.User); perr != nil {
			return nil, true, perr
		}
	}
	return challenge, done, err
}
