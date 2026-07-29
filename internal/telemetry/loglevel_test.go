package telemetry

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/0kaba0hub/yarilo/pkg/logging"
)

func levelFromBody(t *testing.T, body string) string {
	t.Helper()
	var out struct{ Level string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out.Level
}

func TestLogLevelGet(t *testing.T) {
	t.Cleanup(func() { logging.SetLevel(slog.LevelInfo) })
	logging.SetLevel(slog.LevelWarn)

	srv := New(":0")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/loglevel", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := levelFromBody(t, rec.Body.String()); got != "warn" {
		t.Fatalf("level = %q, want \"warn\"", got)
	}
}

// TestLogLevelPostChanges is the point of #889: verbosity changes without a
// restart, which is what makes it usable during an incident.
func TestLogLevelPostChanges(t *testing.T) {
	t.Cleanup(func() { logging.SetLevel(slog.LevelInfo) })
	logging.SetLevel(slog.LevelInfo)

	srv := New(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/debug/loglevel", strings.NewReader(`{"level":"debug"}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := logging.Level(); got != slog.LevelDebug {
		t.Fatalf("active level = %v, want debug", got)
	}
	if got := levelFromBody(t, rec.Body.String()); got != "debug" {
		t.Fatalf("reported level = %q, want \"debug\"", got)
	}
}

func TestLogLevelPostWithTTL(t *testing.T) {
	t.Cleanup(func() { logging.SetLevel(slog.LevelInfo) })
	logging.SetLevel(slog.LevelInfo)

	srv := New(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/debug/loglevel", strings.NewReader(`{"level":"debug","ttl":"50ms"}`))
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := logging.Level(); got != slog.LevelDebug {
		t.Fatalf("active level = %v, want debug during the TTL", got)
	}
}

func TestLogLevelRejectsBadInput(t *testing.T) {
	t.Cleanup(func() { logging.SetLevel(slog.LevelInfo) })
	logging.SetLevel(slog.LevelInfo)

	tests := []struct {
		name string
		body string
	}{
		// An unknown name must be rejected rather than silently accepted as info,
		// which is what ParseLevel does by design for the LOG_LEVEL env var.
		{"unknown level", `{"level":"verbose"}`},
		{"missing level", `{"ttl":"10s"}`},
		{"malformed ttl", `{"level":"debug","ttl":"soon"}`},
		{"negative ttl", `{"level":"debug","ttl":"-5s"}`},
		{"not json", `level=debug`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(":0")
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/debug/loglevel", strings.NewReader(tc.body))
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if got := logging.Level(); got != slog.LevelInfo {
				t.Fatalf("level changed to %v on a rejected request", got)
			}
		})
	}
}

func TestLogLevelMethodNotAllowed(t *testing.T) {
	srv := New(":0")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/debug/loglevel", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Fatal("405 without an Allow header")
	}
}

// TestLogLevelGaugeTracksActiveLevel covers the observability half of #889:
// confirming a change took effect must be possible from the metrics an operator
// is already scraping.
func TestLogLevelGaugeTracksActiveLevel(t *testing.T) {
	t.Cleanup(func() { logging.SetLevel(slog.LevelInfo) })

	srv := New(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/debug/loglevel", strings.NewReader(`{"level":"error"}`))
	srv.Handler().ServeHTTP(rec, req)

	if got := testutil.ToFloat64(logLevelGauge.WithLabelValues("error")); got != float64(slog.LevelError) {
		t.Fatalf("gauge for error = %v, want %v", got, float64(slog.LevelError))
	}
	if n := testutil.CollectAndCount(logLevelGauge); n != 1 {
		t.Fatalf("gauge series = %d, want exactly 1 — stale levels must not linger", n)
	}
}
