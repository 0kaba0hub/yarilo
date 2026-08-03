package jmap

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// trustedServer accepts trustedPeer and nothing else.
func trustedServer(t *testing.T) *Server {
	t.Helper()
	return New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
	})
}

// trustedPeer stands in for the login pod's address.
const trustedPeer = "192.0.2.1:40000"

// The session resource is rendered for the user the login layer asserted, not
// for anything the request derived on its own.
func TestSessionServesTheForwardedUser(t *testing.T) {
	w := httptest.NewRecorder()
	trustedServer(t).Handler().ServeHTTP(w, identityRequest("u1@example.com", trustedPeer))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["username"] != "u1@example.com" {
		t.Errorf("username = %v", got["username"])
	}
	if _, ok := got["apiUrl"]; !ok {
		t.Error("session is missing apiUrl")
	}
}

// The backend runs no passdb chain, so a request without the asserted user is a
// misconfigured hop, not a login failure.
func TestIdentityHeadersAreRequired(t *testing.T) {
	s := trustedServer(t)
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"no user", func(r *http.Request) { r.Header.Del(hdrUser) }, http.StatusForbidden},
		{"blank user", func(r *http.Request) { r.Header.Set(hdrUser, "   ") }, http.StatusForbidden},
		{"bad ttl", func(r *http.Request) { r.Header.Set(hdrProxyTTL, "soon") }, http.StatusForbidden},
		{"ttl exhausted", func(r *http.Request) { r.Header.Set(hdrProxyTTL, "0") }, http.StatusLoopDetected},
		{"no ttl", func(r *http.Request) { r.Header.Del(hdrProxyTTL) }, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := identityRequest("u1@example.com", trustedPeer)
			tt.mutate(r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// Forwarded is the sole source of client origin. X-Forwarded-For is not read,
// so the same fact cannot arrive two ways and disagree.
func TestForwardedIsTheOnlyClientOrigin(t *testing.T) {
	tests := []struct {
		name, header, want string
	}{
		{"ipv4 with port", `for="203.0.113.7:51234";proto=https`, "203.0.113.7"},
		{"ipv4 bare", `for=203.0.113.7;proto=https`, "203.0.113.7"},
		{"ipv6", `for="[2001:db8::1]:443";proto=https`, "2001:db8::1"},
		{"by first", `by="10.1.2.3";for="203.0.113.7:1";proto=https`, "203.0.113.7"},
		{"first hop wins", `for="203.0.113.7:1", for="198.51.100.1:2"`, "203.0.113.7"},
		{"absent", "", ""},
		{"no for member", `proto=https;by="10.1.2.3"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := forwardedFor(tt.header); got != tt.want {
				t.Errorf("forwardedFor(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}

	r := identityRequest("u1@example.com", trustedPeer)
	r.Header.Del(hdrForwarded)
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	id, err := readIdentity(r)
	if err != nil {
		t.Fatalf("readIdentity: %v", err)
	}
	if id.clientIP != "" {
		t.Errorf("client IP = %q, taken from X-Forwarded-For", id.clientIP)
	}
}

// The forward_ passdb fields arrive one header each, percent-encoded.
func TestForwardFieldsAreDecoded(t *testing.T) {
	r := identityRequest("u1@example.com", trustedPeer)
	r.Header.Set(hdrForwardPfx+"Quota", "5%20GB")
	r.Header.Set(hdrForwardPfx+"Tag", "shard-a")
	id, err := readIdentity(r)
	if err != nil {
		t.Fatalf("readIdentity: %v", err)
	}
	want := map[string]string{"quota": "5 GB", "tag": "shard-a"}
	for k, v := range want {
		if id.forward[k] != v {
			t.Errorf("forward[%q] = %q, want %q", k, id.forward[k], v)
		}
	}
}

// The session resource must not be cached: it carries the user's own account.
func TestSessionIsNotCacheable(t *testing.T) {
	w := httptest.NewRecorder()
	trustedServer(t).Handler().ServeHTTP(w, identityRequest("u1@example.com", trustedPeer))
	if cc := w.Header().Get("Cache-Control"); cc == "" || cc == "public" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// A refusal is an RFC 7807 body, which is what a JMAP client parses.
func TestRefusalIsProblemJSON(t *testing.T) {
	s := New(Options{Trust: ResolveTrust(false, false, nil), Limits: testLimits()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, identityRequest("u1@example.com", trustedPeer))
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != jmapcore.ProblemBlank {
		t.Errorf("type = %v", got["type"])
	}
}
