package oauth2

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/emersion/go-sasl"
)

// SASLAuthenticator is the OAUTHBEARER callback, identical to
// sasl.OAuthBearerAuthenticator so callers need not import go-sasl.
type SASLAuthenticator func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError

// XOAuth2SASLAuthenticator is the XOAUTH2 callback: receives the
// (Username, Token) pair from the client's initial response, returns nil on
// success or *sasl.OAuthBearerError on rejection.
type XOAuth2SASLAuthenticator func(opts sasl.XOAuth2Options) *sasl.OAuthBearerError

// NewOAuthBearerSASLServer returns an OAUTHBEARER server with fast-fail
// semantics: on rejection it returns the error JSON with done=true instead of
// the RFC 7628 §3.2.3 dummy-0x01 round-trip. Common clients never send the
// 0x01 acknowledgement, which would leave the server blocking on a read
// until the protocol deadline. Success behaviour matches upstream
// sasl.NewOAuthBearerServer.
func NewOAuthBearerSASLServer(auth SASLAuthenticator) sasl.Server {
	return &fastFailOAuthBearerServer{authenticate: auth}
}

type fastFailOAuthBearerServer struct {
	done         bool
	authenticate SASLAuthenticator
}

func (s *fastFailOAuthBearerServer) Next(response []byte) ([]byte, bool, error) {
	if s.done {
		return nil, true, errFastFailDone
	}

	// No initial response: send an empty challenge, matching upstream
	// go-sasl.
	if response == nil {
		return []byte{}, false, nil
	}

	s.done = true

	opts, err := parseOAuthBearerInitial(response)
	if err != nil {
		blob, _ := json.Marshal(sasl.OAuthBearerError{
			Status:  "invalid_request",
			Schemes: "bearer",
		})
		return blob, true, err
	}

	if authzErr := s.authenticate(opts); authzErr != nil {
		blob, jerr := json.Marshal(authzErr)
		if jerr != nil {
			return nil, true, jerr
		}
		// done=true skips the RFC 7628 §3.2.3 dummy round-trip.

		return blob, true, authzErr
	}
	return nil, true, nil
}

var errFastFailDone = errors.New("sasl/oauthbearer: Next called after done")

// NewXOAuth2SASLServer returns an XOAUTH2 SASL server with the
// same fast-fail semantics as NewOAuthBearerSASLServer. The go-
// sasl fork's ParseXOAuth2Initial handles wire decoding; this
// wrapper maps the result into the session-layer authenticator
// and returns done=true immediately on both success and failure.
func NewXOAuth2SASLServer(auth XOAuth2SASLAuthenticator) sasl.Server {
	return &fastFailXOAuth2Server{authenticate: auth}
}

type fastFailXOAuth2Server struct {
	done         bool
	authenticate XOAuth2SASLAuthenticator
}

func (s *fastFailXOAuth2Server) Next(response []byte) ([]byte, bool, error) {
	if s.done {
		return nil, true, errors.New("sasl/xoauth2: Next called after done")
	}
	if response == nil {
		return []byte{}, false, nil
	}
	s.done = true

	opts, err := sasl.ParseXOAuth2Initial(response)
	if err != nil {
		blob, _ := json.Marshal(sasl.OAuthBearerError{
			Status:  "invalid_request",
			Schemes: "bearer",
		})
		return blob, true, err
	}
	if authzErr := s.authenticate(opts); authzErr != nil {
		blob, jerr := json.Marshal(authzErr)
		if jerr != nil {
			return nil, true, jerr
		}
		return blob, true, authzErr
	}
	return nil, true, nil
}

// parseOAuthBearerInitial extracts (gs2-flag, authzid, key=value
// pairs) from the OAUTHBEARER initial response. Returns
// OAuthBearerOptions with Username + Token + Host + Port
// populated when present.
//
// Wire shape (RFC 7628 §3.1):
//
//	"n,a=user@example.com,\x01host=mail.example.com\x01port=993\x01auth=Bearer <token>\x01\x01"
//
// We accept the spec's lenient ordering: any number of key=value
// segments separated by 0x01, terminated by two consecutive 0x01.
// We do not enforce strict ordering — operators have hit clients
// in the wild that reorder fields.
func parseOAuthBearerInitial(resp []byte) (sasl.OAuthBearerOptions, error) {
	var opts sasl.OAuthBearerOptions
	parts := bytes.SplitN(resp, []byte{','}, 3)
	if len(parts) != 3 {
		return opts, errors.New("oauth2/sasl: malformed initial response (gs2 prefix)")
	}
	if !bytes.Equal(parts[0], []byte{'n'}) {
		return opts, errors.New("oauth2/sasl: gs2-cb-flag must be 'n' (channel binding not supported)")
	}
	if authzid := parts[1]; len(authzid) > 0 {
		if !bytes.HasPrefix(authzid, []byte("a=")) {
			return opts, errors.New("oauth2/sasl: gs2-authzid must use a= prefix")
		}
		opts.Username = string(bytes.TrimPrefix(authzid, []byte("a=")))
	}

	for _, kv := range bytes.Split(parts[2], []byte{0x01}) {
		if len(kv) == 0 {
			continue
		}
		eq := bytes.IndexByte(kv, '=')
		if eq < 0 {
			return opts, errors.New("oauth2/sasl: malformed key=value pair")
		}
		key := string(kv[:eq])
		value := string(kv[eq+1:])
		switch key {
		case "host":
			opts.Host = value
		case "port":
			n, err := strconv.ParseUint(value, 10, 16)
			if err != nil {
				return opts, errors.New("oauth2/sasl: malformed port")
			}
			opts.Port = int(n)
		case "auth":
			const prefix = "bearer "
			if !strings.HasPrefix(strings.ToLower(value), prefix) {
				return opts, errors.New("oauth2/sasl: unsupported token type (only Bearer)")
			}
			opts.Token = value[len(prefix):]
		}
	}
	return opts, nil
}
