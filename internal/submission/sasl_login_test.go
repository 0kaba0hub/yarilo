package submission

import (
	"errors"
	"testing"
)

// With SASL-IR (go-sasl LoginClient default): client sends username as the
// initial response, expects only the Password: challenge.
func TestLoginServer_HappyPath_WithInitialResponse(t *testing.T) {
	var gotUser, gotPass string
	srv := newLoginServer(func(u, p string) error {
		gotUser, gotPass = u, p
		return nil
	})

	chal, done, err := srv.Next([]byte("alice@example.com"))
	if err != nil || done {
		t.Fatalf("step 1: err=%v done=%v", err, done)
	}
	if string(chal) != "Password:" {
		t.Fatalf("step 1 challenge: got %q, want Password:", chal)
	}

	_, done, err = srv.Next([]byte("secret"))
	if err != nil || !done {
		t.Fatalf("step 2: err=%v done=%v", err, done)
	}
	if gotUser != "alice@example.com" || gotPass != "secret" {
		t.Fatalf("authenticator got (%q,%q)", gotUser, gotPass)
	}
}

// Legacy LOGIN without initial response: client uses bare "AUTH LOGIN",
// server prompts Username:, then Password:.
func TestLoginServer_HappyPath_LegacyNoInitialResponse(t *testing.T) {
	var gotUser, gotPass string
	srv := newLoginServer(func(u, p string) error {
		gotUser, gotPass = u, p
		return nil
	})

	chal, done, err := srv.Next(nil)
	if err != nil || done {
		t.Fatalf("step 1: err=%v done=%v", err, done)
	}
	if string(chal) != "Username:" {
		t.Fatalf("step 1 challenge: got %q, want Username:", chal)
	}

	chal, done, err = srv.Next([]byte("alice@example.com"))
	if err != nil || done {
		t.Fatalf("step 2: err=%v done=%v", err, done)
	}
	if string(chal) != "Password:" {
		t.Fatalf("step 2 challenge: got %q, want Password:", chal)
	}

	_, done, err = srv.Next([]byte("secret"))
	if err != nil || !done {
		t.Fatalf("step 3: err=%v done=%v", err, done)
	}
	if gotUser != "alice@example.com" || gotPass != "secret" {
		t.Fatalf("authenticator got (%q,%q)", gotUser, gotPass)
	}
}

func TestLoginServer_AuthFails(t *testing.T) {
	srv := newLoginServer(func(_, _ string) error {
		return errors.New("bad creds")
	})
	_, _, _ = srv.Next([]byte("alice"))
	_, done, err := srv.Next([]byte("wrong"))
	if !done {
		t.Fatal("expected done after auth attempt")
	}
	if err == nil {
		t.Fatal("expected auth error from authenticator to surface")
	}
}

func TestLoginServer_MissingResponses(t *testing.T) {
	srv := newLoginServer(func(_, _ string) error { return nil })
	_, _, _ = srv.Next(nil)
	if _, _, err := srv.Next(nil); err == nil {
		t.Fatal("expected error when username response is nil")
	}

	srv = newLoginServer(func(_, _ string) error { return nil })
	_, _, _ = srv.Next(nil)
	_, _, _ = srv.Next([]byte("alice"))
	if _, _, err := srv.Next(nil); err == nil {
		t.Fatal("expected error when password response is nil")
	}
}
