package oauth2

import (
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
)

// TestSASLServer_HappyPath — well-formed initial response gives
// the callback the parsed (Username, Token, Host, Port) and
// finishes with done=true.
func TestSASLServer_HappyPath(t *testing.T) {
	var gotOpts sasl.OAuthBearerOptions
	srv := NewOAuthBearerSASLServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		gotOpts = opts
		return nil
	})
	resp := []byte("n,a=alice@example.com,\x01host=mail.example.com\x01port=993\x01auth=Bearer my-token\x01\x01")
	challenge, done, err := srv.Next(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Errorf("expected done=true on success")
	}
	if len(challenge) != 0 {
		t.Errorf("expected empty challenge on success, got %q", challenge)
	}
	if gotOpts.Username != "alice@example.com" {
		t.Errorf("Username = %q", gotOpts.Username)
	}
	if gotOpts.Token != "my-token" {
		t.Errorf("Token = %q", gotOpts.Token)
	}
	if gotOpts.Host != "mail.example.com" || gotOpts.Port != 993 {
		t.Errorf("Host/Port = %q/%d", gotOpts.Host, gotOpts.Port)
	}
}

// TestSASLServer_FastFailFinishesImmediately — the whole reason
// this wrapper exists. Callback rejection must terminate the SASL
// exchange on the first call (done=true) rather than entering the
// dummy-0x01 acknowledgement dance.
func TestSASLServer_FastFailFinishesImmediately(t *testing.T) {
	srv := NewOAuthBearerSASLServer(func(_ sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
	})
	resp := []byte("n,a=x,\x01auth=Bearer tkn\x01\x01")
	challenge, done, err := srv.Next(resp)
	if !done {
		t.Errorf("fast-fail must set done=true on first call")
	}
	if err == nil {
		t.Errorf("fast-fail must return an error")
	}
	// Challenge carries the JSON error blob so the protocol layer
	// surfaces it to the client.
	if !strings.Contains(string(challenge), `"status":"invalid_token"`) {
		t.Errorf("challenge missing JSON error: %q", challenge)
	}
}

// TestSASLServer_NilInitialResponse — SASL framework signals
// "expecting initial response" by passing nil; server replies
// with an empty challenge and done=false.
func TestSASLServer_NilInitialResponse(t *testing.T) {
	srv := NewOAuthBearerSASLServer(func(_ sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		t.Errorf("callback fired on nil response")
		return nil
	})
	challenge, done, err := srv.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Errorf("nil response should not finish")
	}
	if len(challenge) != 0 {
		t.Errorf("expected empty challenge, got %q", challenge)
	}
}

// TestSASLServer_MalformedRejected — broken wire format
// (no gs2 prefix) fast-fails with invalid_request JSON.
func TestSASLServer_MalformedRejected(t *testing.T) {
	srv := NewOAuthBearerSASLServer(func(_ sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		t.Errorf("callback fired on malformed response")
		return nil
	})
	challenge, done, err := srv.Next([]byte("garbage"))
	if !done || err == nil {
		t.Errorf("malformed input should fast-fail; done=%v err=%v", done, err)
	}
	if !strings.Contains(string(challenge), "invalid_request") {
		t.Errorf("expected invalid_request JSON in challenge: %q", challenge)
	}
}

// TestParseInitial_GS2WithChannelBindingRejected — RFC 7628
// forbids OAUTHBEARER channel binding for now. Anything other
// than "n" in the gs2-cb-flag is rejected.
func TestParseInitial_GS2WithChannelBindingRejected(t *testing.T) {
	_, err := parseOAuthBearerInitial([]byte("p=tls-server-end-point,a=x,\x01auth=Bearer t\x01\x01"))
	if err == nil {
		t.Errorf("p= gs2-cb-flag should be rejected")
	}
}

// TestParseInitial_AcceptsReorderedFields — operator-side reports
// of clients that reorder host/port/auth. The wrapper accepts any
// order, just extracts what it knows.
func TestParseInitial_AcceptsReorderedFields(t *testing.T) {
	resp := []byte("n,a=user,\x01auth=Bearer t\x01port=143\x01host=imap.x\x01\x01")
	opts, err := parseOAuthBearerInitial(resp)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Token != "t" || opts.Host != "imap.x" || opts.Port != 143 {
		t.Errorf("reordered parse wrong: %+v", opts)
	}
}

// TestParseInitial_EmptyAuthzid — RFC 7628 allows the authzid
// field to be empty (token's claim resolves the identity).
func TestParseInitial_EmptyAuthzid(t *testing.T) {
	opts, err := parseOAuthBearerInitial([]byte("n,,\x01auth=Bearer t\x01\x01"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Username != "" {
		t.Errorf("empty authzid: Username = %q", opts.Username)
	}
	if opts.Token != "t" {
		t.Errorf("token lost: %q", opts.Token)
	}
}

// TestParseInitial_TokenTypeMustBeBearer — RFC 7628 §3.1 — only
// Bearer tokens. Other token types (MAC, etc.) rejected.
func TestParseInitial_TokenTypeMustBeBearer(t *testing.T) {
	_, err := parseOAuthBearerInitial([]byte("n,a=x,\x01auth=MAC sometoken\x01\x01"))
	if err == nil {
		t.Errorf("non-Bearer token type should be rejected")
	}
}

// TestSASLServer_DoubleNextErrors — Next called twice (after
// done=true) returns an error so a buggy protocol loop fails
// loudly instead of looping silently.
func TestSASLServer_DoubleNextErrors(t *testing.T) {
	srv := NewOAuthBearerSASLServer(func(_ sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		return nil
	})
	srv.Next([]byte("n,a=x,\x01auth=Bearer t\x01\x01")) //nolint:errcheck
	_, _, err := srv.Next([]byte("anything"))
	if !errors.Is(err, errFastFailDone) {
		t.Errorf("double-next: err=%v, want errFastFailDone", err)
	}
}
