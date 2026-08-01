package ftsservice

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/0kaba0hub/yarilo/internal/telemetry"
)

// TestMetricsIncrement verifies the FTS metrics are registered and move.
func TestMetricsIncrement(t *testing.T) {
	before := testutil.ToFloat64(metricLookupTotal)
	metricLookupTotal.Inc()
	if got := testutil.ToFloat64(metricLookupTotal); got != before+1 {
		t.Fatalf("fts_lookup_total = %v, want %v", got, before+1)
	}
	// Must not panic and must record a sample.
	ObserveLockWait(2 * time.Millisecond)
	metricRecoveryTotal.WithLabelValues("database_closed").Inc()
	metricQueueDepth.Set(3)
	if got := testutil.ToFloat64(metricQueueDepth); got != 3 {
		t.Fatalf("fts_index_queue_depth = %v, want 3", got)
	}
}

// TestMetricsEndpointExposesFTSMetrics checks the telemetry /metrics
// handler serves the FTS metric families.
func TestMetricsEndpointExposesFTSMetrics(t *testing.T) {
	// Touch each family so even the label-vec ones have a series to render.
	metricLookupTotal.Inc()
	metricIndexMessages.Add(1)
	metricRecoveryTotal.WithLabelValues("rev_file").Inc()
	ObserveLockWait(time.Millisecond)

	srv := telemetry.New(":0")
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"fts_lookup_total",
		"fts_lookup_duration_seconds",
		"fts_lookup_candidates",
		"fts_index_messages_total",
		"fts_index_duration_seconds",
		"fts_index_queue_depth",
		"fts_recovery_total",
		"fts_lock_wait_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics missing %s", name)
		}
	}
}
