// Package scram adapts the go-sasl SCRAM servers for session use:
// it captures the authenticated username (the sasl factory doesn't
// expose it) and runs a post-success hook before the connection is
// marked authenticated. Wire semantics are unchanged.
package scram

import (
	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// Session is a sasl.Server wrapper driven via Next().
type Session struct {
	inner sasl.Server

	// User is set when the lookup callback first fires.
	User string

	// OnSuccess runs after the exchange finishes cleanly; a non-nil
	// return is surfaced to the client as auth failure.
	OnSuccess func(user string) error
}

// NewSha256 builds a SCRAM-SHA-256 session adapter. nil onSuccess
// disables the hook.
func NewSha256(lookup protocol.SCRAMSha256Lookup, onSuccess func(string) error) *Session {
	s := &Session{OnSuccess: onSuccess}
	s.inner = sasl.NewScramSha256Server(func(user string) (*sasl.ScramCredentials, error) {
		s.User = user
		return lookup.LookupSCRAMSha256(user)
	})
	return s
}

// NewSha256Plus is the channel-bound variant. cbData is the TLS
// exporter output (RFC 9266) used for the c= verification.
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

// NewSha1 builds the SHA-1 variant of NewSha256.
func NewSha1(lookup protocol.SCRAMSha1Lookup, onSuccess func(string) error) *Session {
	s := &Session{OnSuccess: onSuccess}
	s.inner = sasl.NewScramSha1Server(func(user string) (*sasl.ScramCredentials, error) {
		s.User = user
		return lookup.LookupSCRAMSha1(user)
	})
	return s
}

// NewSha1Plus is the channel-bound SHA-1 variant.
func NewSha1Plus(lookup protocol.SCRAMSha1Lookup, cbData []byte, onSuccess func(string) error) *Session {
	s := &Session{OnSuccess: onSuccess}
	s.inner = sasl.NewScramSha1PlusServer(
		func(user string) (*sasl.ScramCredentials, error) {
			s.User = user
			return lookup.LookupSCRAMSha1(user)
		},
		sasl.ScramServerOptions{ChannelBindingData: cbData},
	)
	return s
}

// Next satisfies sasl.Server; runs OnSuccess after a clean finish.
func (s *Session) Next(response []byte) (challenge []byte, done bool, err error) {
	challenge, done, err = s.inner.Next(response)
	if done && err == nil && s.OnSuccess != nil {
		if perr := s.OnSuccess(s.User); perr != nil {
			return nil, true, perr
		}
	}
	return challenge, done, err
}
