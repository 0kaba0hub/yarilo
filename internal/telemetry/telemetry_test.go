package telemetry_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/telemetry"
)

func newTestServer(t *testing.T) (*telemetry.Server, *httptest.Server) {
	t.Helper()
	s := telemetry.New("")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)

	code, body := getBody(t, ts.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}
	if body != "ok" {
		t.Fatalf("body: got %q, want %q", body, "ok")
	}
}

func TestReadyz_NotReady(t *testing.T) {
	_, ts := newTestServer(t)

	code, _ := getBody(t, ts.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", code)
	}
}

func TestReadyz_Ready(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetReady(true)

	code, body := getBody(t, ts.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}
	// The body is JSON naming every condition, so a failing probe says which
	// dependency is missing instead of only that the pod is not ready. Nothing
	// consumes the old "ok" text: kubelet probes look at the status code and
	// smoketest only prints the body on failure.
	if !strings.Contains(body, `"ready":true`) {
		t.Fatalf("body: got %q, want it to report ready:true", body)
	}
}

func TestMetrics(t *testing.T) {
	_, ts := newTestServer(t)

	code, _ := getBody(t, ts.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status: got %d, want 200", code)
	}
}

func TestReadyzToggle(t *testing.T) {
	s, ts := newTestServer(t)

	cases := []struct {
		ready    bool
		wantCode int
	}{
		{ready: true, wantCode: http.StatusOK},
		{ready: false, wantCode: http.StatusServiceUnavailable},
		{ready: true, wantCode: http.StatusOK},
	}

	for _, tc := range cases {
		s.SetReady(tc.ready)
		code, _ := getBody(t, ts.URL+"/readyz")
		if code != tc.wantCode {
			t.Fatalf("SetReady(%v): got %d, want %d", tc.ready, code, tc.wantCode)
		}
	}
}
