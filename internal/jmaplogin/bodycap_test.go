package jmaplogin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// post sends a body to the proxy as an authenticated client would.
func post(t *testing.T, base, authz, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, base+"/jmap/api/", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := keepAliveClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// The cap is enforced at the edge so an oversized body never reaches a backend,
// and the refusal is a JMAP problem the client can parse rather than a bare 413.
func TestBodyCapRefusesAtTheEdge(t *testing.T) {
	wd := &fakeWarden{}
	base := startProxy(t, Options{Warden: wd, MaxSizeRequest: 128})

	tests := []struct {
		name string
		body string
		want int
	}{
		{"under the cap", `{"using":[],"methodCalls":[]}`, http.StatusOK},
		{"over the cap", `{"pad":"` + strings.Repeat("x", 200) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, base, basic("u1", "pw"), tt.body)
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want != http.StatusRequestEntityTooLarge {
				return
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var got map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["type"] != jmapcore.ProblemLimit {
				t.Errorf("type = %v, want %s", got["type"], jmapcore.ProblemLimit)
			}
			// §3.6.1 requires the limit member, and it must name the bound.
			if got["limit"] != "maxSizeRequest" {
				t.Errorf("limit = %v, want maxSizeRequest", got["limit"])
			}
		})
	}
}

// A cap of zero means unlimited, so an existing deployment that never set the
// key does not start refusing traffic on upgrade.
func TestBodyCapZeroIsUnlimited(t *testing.T) {
	base := startProxy(t, Options{})
	resp := post(t, base, basic("u1", "pw"), `{"pad":"`+strings.Repeat("x", 10_000)+`"}`)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// The refusal happens after authentication: an anonymous caller must not be
// able to learn the limit, and 401 stays the first thing it sees.
func TestBodyCapDoesNotPrecedeAuth(t *testing.T) {
	base := startProxy(t, Options{MaxSizeRequest: 128})
	resp := post(t, base, "", `{"pad":"`+strings.Repeat("x", 200)+`"}`)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
