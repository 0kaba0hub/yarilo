package jmaplogin

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The backend trusts these headers, so the proxy must set them itself and
// never let a caller's copy survive.
func TestProxySetsTheContractHeaders(t *testing.T) {
	base := startProxy(t, Options{LocalIP: "10.1.2.3"})
	resp := get(t, keepAliveClient(), base, basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Echo-" + hdrUser); got != "u1" {
		t.Errorf("%s = %q, want the authenticated user", hdrUser, got)
	}
	if got := resp.Header.Get("Echo-" + hdrSessionID); got == "" {
		t.Errorf("%s is empty", hdrSessionID)
	}
	if got := resp.Header.Get("Echo-" + hdrProxyTTL); got != "4" {
		t.Errorf("%s = %q, want 4 (5 minus this hop)", hdrProxyTTL, got)
	}
	fwd := resp.Header.Get("Echo-" + hdrForwarded)
	for _, want := range []string{`for="`, "proto=http", `by="10.1.2.3"`} {
		if !strings.Contains(fwd, want) {
			t.Errorf("%s = %q, want it to contain %q", hdrForwarded, fwd, want)
		}
	}
}

// A caller naming itself must not reach the backend. The backend's trust rule
// is the second line of defence; this is the first.
func TestProxyStripsClientSuppliedIdentity(t *testing.T) {
	base := startProxy(t, Options{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", basic("u1", "pw"))
	req.Header.Set(hdrUser, "admin")
	req.Header.Set(hdrSessionID, "forged")
	req.Header.Set(hdrForwarded, `for="203.0.113.9"`)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	resp, err := keepAliveClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if got := resp.Header.Get("Echo-" + hdrUser); got != "u1" {
		t.Errorf("%s = %q, a forged identity survived", hdrUser, got)
	}
	if got := resp.Header.Get("Echo-" + hdrSessionID); got == "forged" {
		t.Error("a forged session id survived")
	}
	if got := resp.Header.Get("Echo-" + hdrForwarded); strings.Contains(got, "203.0.113.9") {
		t.Errorf("%s = %q, a forged origin survived", hdrForwarded, got)
	}
}

// The hop budget decrements, so a proxy loop terminates instead of spinning.
func TestProxyDecrementsTheHopBudget(t *testing.T) {
	base := startProxy(t, Options{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", basic("u1", "pw"))

	resp, err := keepAliveClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if got := resp.Header.Get("Echo-" + hdrProxyTTL); got != "4" {
		t.Errorf("first hop TTL = %q, want 4", got)
	}
}

// A router that cannot place the user answers a gateway error rather than a
// blank failure, and never falls back to some arbitrary pod.
func TestProxyReportsAnUnplaceableUser(t *testing.T) {
	base := startProxy(t, Options{Router: StaticRouter{}})
	resp := get(t, keepAliveClient(), base, basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// The contract has an open-ended X-Yarilo-Forward-<key> family, so stripping a
// fixed list would leave a way to inject passdb forward fields into the backend.
func TestProxyStripsTheWholeYariloPrefix(t *testing.T) {
	base := startProxy(t, Options{})
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	r.Header.Set("Authorization", basic("u1", "pw"))
	r.Header.Set("X-Yarilo-Forward-x", "injected")
	r.Header.Set("X-Yarilo-Anything", "injected")

	resp, err := keepAliveClient().Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	for _, h := range []string{"X-Yarilo-Forward-x", "X-Yarilo-Anything"} {
		if got := resp.Header.Get("Echo-Extra-" + h); got != "" {
			t.Errorf("%s survived as %q", h, got)
		}
	}
}

// The login layer is the first hop by definition, so the budget it emits comes
// from the constant and never from the caller, who could otherwise widen it.
func TestProxyIgnoresAClientSuppliedHopBudget(t *testing.T) {
	base := startProxy(t, Options{})
	for _, sent := range []string{"5", "99", "0", "-3", "garbage"} {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		r.Header.Set("Authorization", basic("u1", "pw"))
		r.Header.Set(hdrProxyTTL, sent)

		resp, err := keepAliveClient().Do(r)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		got := resp.Header.Get("Echo-" + hdrProxyTTL)
		resp.Body.Close() //nolint:errcheck
		if got != firstHopTTL {
			t.Errorf("client sent %q, backend saw %q, want %s", sent, got, firstHopTTL)
		}
	}
}
