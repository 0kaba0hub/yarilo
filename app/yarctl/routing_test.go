package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveBackendUser(t *testing.T) {
	tests := []struct {
		name string
		path string
		body any
		want string
	}{
		{"get query user", "/api/backend/fts/status?user=u1&folder=INBOX", nil, "u1"},
		{"post body user", "/api/backend/folder/create", map[string]any{"user": "u2", "folder": "X"}, "u2"},
		{"body wins over empty query", "/api/backend/quota/recalc", map[string]any{"user": "u3"}, "u3"},
		{"no user (global)", "/api/backend/dict/foo/lookup", struct{ Key string }{"k"}, ""},
		{"no user query", "/api/backend/who/count", map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBackendUser(tt.path, tt.body); got != tt.want {
				t.Errorf("resolveBackendUser(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestBackendBaseForUser(t *testing.T) {
	// Save/restore package globals the resolver reads.
	oldRoute, oldAPI, oldBackend, oldPort := routeByUser, apiURL, backendAPIURL, backendAPIPort
	defer func() { routeByUser, apiURL, backendAPIURL, backendAPIPort = oldRoute, oldAPI, oldBackend, oldPort }()
	backendAPIURL = "http://fixed-backend:9105"
	backendAPIPort = 9105

	t.Run("routing off returns the fixed backend url", func(t *testing.T) {
		routeByUser = false
		got, err := backendBaseForUser("u1")
		if err != nil || got != backendAPIURL {
			t.Fatalf("got %q err=%v, want %q", got, err, backendAPIURL)
		}
	})

	t.Run("no user returns the fixed backend url even when routing on", func(t *testing.T) {
		routeByUser = true
		got, err := backendBaseForUser("")
		if err != nil || got != backendAPIURL {
			t.Fatalf("got %q err=%v, want %q", got, err, backendAPIURL)
		}
	})

	t.Run("routing on resolves the user's pod via director LOOKUP", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/director/map" || r.URL.Query().Get("user") != "u1" {
				http.Error(w, "unexpected", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"user":"u1","backend":"10.1.2.3","port":0,"tag":"","sticky":true}`)) //nolint:errcheck
		}))
		defer srv.Close()
		routeByUser = true
		apiURL = srv.URL

		got, err := backendBaseForUser("u1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "http://10.1.2.3:9105" {
			t.Fatalf("got %q, want http://10.1.2.3:9105", got)
		}
	})

	t.Run("director down yields a clean error, never a silent fallback", func(t *testing.T) {
		routeByUser = true
		apiURL = "http://127.0.0.1:1" // nothing listening
		got, err := backendBaseForUser("u1")
		if err == nil {
			t.Fatalf("want error when director is unreachable, got base %q", got)
		}
		if got != "" {
			t.Fatalf("must not fall back to a base; got %q", got)
		}
	})

	t.Run("director returns no backend yields a clean error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"user":"u1","backend":""}`)) //nolint:errcheck
		}))
		defer srv.Close()
		routeByUser = true
		apiURL = srv.URL
		if _, err := backendBaseForUser("u1"); err == nil {
			t.Fatal("want error when director returns no backend")
		}
	})
}

// TestBackendDispatchRoutesWarden is the #953 regression: in the backend-api
// container (YARILO_ADMIN_TYPE=backend) `yarctl warden dump` routes through
// dispatchBackend, so `warden` must resolve there — not fail as an unknown
// backend service.
func TestBackendDispatchRoutesWarden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/backend/warden/dump" {
			w.Write([]byte(`{"counters":[],"penalties":[]}`)) //nolint:errcheck
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	old := backendAPIURL
	defer func() { backendAPIURL = old }()
	backendAPIURL = srv.URL

	if err := dispatchBackend([]string{"warden", "dump", "--output", "json"}); err != nil {
		t.Fatalf("dispatchBackend routing warden dump: %v", err)
	}
}
