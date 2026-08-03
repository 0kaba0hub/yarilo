package jmap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/oauth2"
)

type fakeAuth struct {
	user, pass string
	lastIP     string
}

func (f *fakeAuth) Authenticate(username, password, _, remoteIP, _ string) (string, error) {
	f.lastIP = remoteIP
	if username == f.user && password == f.pass {
		return username, nil
	}
	return "", errors.New("bad credentials")
}

type fakeValidator struct {
	token string
	user  string
}

func (f *fakeValidator) Validate(_ context.Context, token string) (*oauth2.Claims, error) {
	if token == f.token {
		return &oauth2.Claims{Username: f.user}, nil
	}
	return nil, errors.New("bad token")
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Protocol.BaseURL == "" {
		opts.Protocol = testProtocol()
	}
	return New(opts)
}

func do(t *testing.T, s *Server, authz string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	req.RemoteAddr = "10.1.2.3:44444"
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Result()
}

// The session resource is the only thing an unauthenticated client could learn
// about a deployment, so it is behind auth like every other route.
func TestSessionRequiresCredentials(t *testing.T) {
	s := newTestServer(t, Options{Auth: &fakeAuth{user: "u1", pass: "pw"}})
	tests := []struct {
		name, authz string
	}{
		{"no header", ""},
		{"unknown scheme", "Digest abc"},
		{"empty credential", "Basic "},
		{"malformed basic", "Basic !!!not-base64!!!"},
		{"basic without colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("u1"))},
		{"wrong password", basic("u1", "nope")},
		{"unknown user", basic("nobody", "pw")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := do(t, s, tt.authz)
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate header")
			}
		})
	}
}

func TestSessionAcceptsBasic(t *testing.T) {
	auth := &fakeAuth{user: "u1@example.com", pass: "pw"}
	s := newTestServer(t, Options{Auth: auth})

	resp := do(t, s, basic("u1@example.com", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["username"] != "u1@example.com" {
		t.Errorf("username = %v", got["username"])
	}
	// The passdb decides on allow_nets, so it has to see the real peer.
	if auth.lastIP != "10.1.2.3" {
		t.Errorf("passdb saw remote IP %q", auth.lastIP)
	}
}

func TestSessionAcceptsBearer(t *testing.T) {
	s := newTestServer(t, Options{
		TokenValidator: &fakeValidator{token: "tok", user: "u2@example.com"},
	})
	resp := do(t, s, "Bearer tok")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["username"] != "u2@example.com" {
		t.Errorf("username = %v", got["username"])
	}
}

// A rejected Bearer must not fall through to Basic: the client meant the token,
// and retrying as a password would report the wrong failure.
func TestBearerDoesNotFallBackToBasic(t *testing.T) {
	auth := &fakeAuth{user: "u1", pass: "pw"}
	s := newTestServer(t, Options{
		Auth:           auth,
		TokenValidator: &fakeValidator{token: "good", user: "u1"},
	})
	resp := do(t, s, "Bearer wrong")
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if auth.lastIP != "" {
		t.Error("a rejected token reached the password authenticator")
	}
}

// A credential type with nothing wired must be refused, not panic.
func TestUnwiredCredentialTypesAreRefused(t *testing.T) {
	s := newTestServer(t, Options{})
	for _, authz := range []string{"Bearer tok", basic("u1", "pw")} {
		resp := do(t, s, authz)
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", authz, resp.StatusCode)
		}
	}
}

// Plaintext credentials on a cleartext connection are refused when the listener
// says so, matching the other protocols.
func TestBasicRefusedWithoutTLSWhenDisabled(t *testing.T) {
	s := newTestServer(t, Options{
		Auth:             &fakeAuth{user: "u1", pass: "pw"},
		DisablePlainAuth: true,
	})
	resp := do(t, s, basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// A 401 body must not hint whether the user exists.
func TestUnauthorizedBodyLeaksNothing(t *testing.T) {
	s := newTestServer(t, Options{Auth: &fakeAuth{user: "u1", pass: "pw"}})
	for _, authz := range []string{basic("u1", "wrong"), basic("ghost", "wrong")} {
		resp := do(t, s, authz)
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close() //nolint:errcheck
		if body["detail"] != "Unauthorized" {
			t.Errorf("detail = %v, want a fixed string", body["detail"])
		}
	}
}

// Only the well-known path is served in this phase; the API routes arrive later
// and must not 200 by accident in the meantime.
func TestOnlySessionRouteExists(t *testing.T) {
	s := newTestServer(t, Options{Auth: &fakeAuth{user: "u1", pass: "pw"}})
	for _, path := range []string{"/jmap/api/", "/jmap/upload/", "/", "/.well-known/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", basic("u1", "pw"))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("%s answered 200 before it is implemented", path)
		}
	}
}
