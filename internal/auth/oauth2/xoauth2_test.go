package oauth2

import (
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
)

func TestXOAuth2SASLServer_HappyPath(t *testing.T) {
	var gotOpts sasl.XOAuth2Options
	srv := NewXOAuth2SASLServer(func(opts sasl.XOAuth2Options) *sasl.OAuthBearerError {
		gotOpts = opts
		return nil
	})
	resp := []byte("user=alice@example.com\x01auth=Bearer ya29.token\x01\x01")
	_, done, err := srv.Next(resp)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if gotOpts.Username != "alice@example.com" {
		t.Errorf("Username = %q", gotOpts.Username)
	}
	if gotOpts.Token != "ya29.token" {
		t.Errorf("Token = %q", gotOpts.Token)
	}
}

func TestXOAuth2SASLServer_FastFailFinishesImmediately(t *testing.T) {
	srv := NewXOAuth2SASLServer(func(_ sasl.XOAuth2Options) *sasl.OAuthBearerError {
		return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
	})
	resp := []byte("user=alice@example.com\x01auth=Bearer bad\x01\x01")
	challenge, done, err := srv.Next(resp)
	if !done {
		t.Errorf("fast-fail must set done=true")
	}
	if err == nil {
		t.Errorf("fast-fail must return error")
	}
	if !strings.Contains(string(challenge), `"status":"invalid_token"`) {
		t.Errorf("JSON error missing: %q", challenge)
	}
}

func TestXOAuth2SASLServer_NilInitialResponse(t *testing.T) {
	srv := NewXOAuth2SASLServer(func(_ sasl.XOAuth2Options) *sasl.OAuthBearerError {
		t.Errorf("callback fired on nil response")
		return nil
	})
	challenge, done, err := srv.Next(nil)
	if err != nil || done {
		t.Fatalf("nil: done=%v err=%v", done, err)
	}
	if len(challenge) != 0 {
		t.Errorf("expected empty challenge, got %q", challenge)
	}
}

func TestXOAuth2SASLServer_MalformedRejected(t *testing.T) {
	srv := NewXOAuth2SASLServer(func(_ sasl.XOAuth2Options) *sasl.OAuthBearerError {
		t.Errorf("callback fired on malformed response")
		return nil
	})
	challenge, done, err := srv.Next([]byte("garbage-no-x01"))
	if !done || err == nil {
		t.Errorf("malformed: done=%v err=%v", done, err)
	}
	if !strings.Contains(string(challenge), "invalid_request") {
		t.Errorf("expected invalid_request JSON: %q", challenge)
	}
}
