package anvil

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveRequestLabels(t *testing.T) {
	tests := []struct {
		name   string
		verb   string
		result string
	}{
		{"connect ok", "CONNECT", "ok"},
		{"connect refused", "CONNECT", "too_many_connections"},
		{"heartbeat ok", "HEARTBEAT", "ok"},
		{"heartbeat reaped", "HEARTBEAT", "session_unknown"},
		{"lookup", "LOOKUP", "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.CollectAndCount(requestSeconds)
			observeRequest(tc.verb, tc.result, time.Now())
			if got := testutil.CollectAndCount(requestSeconds); got < before {
				t.Fatalf("series count shrank: %d → %d", before, got)
			}
		})
	}
}

func TestPenaltyLookupTaxonomy(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		{"no entry", "miss"},
		{"penalty in force", "hit"},
		{"decayed entry", "expired"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(penaltyLookups.WithLabelValues(tc.result))
			penaltyLookups.WithLabelValues(tc.result).Inc()
			if got := testutil.ToFloat64(penaltyLookups.WithLabelValues(tc.result)); got != before+1 {
				t.Fatalf("%s counter = %v, want %v", tc.result, got, before+1)
			}
		})
	}
}

// TestSweepReportsReapedSessions covers the signal that explains a
// reason=unknown HEARTBEAT: the sweeper dropped a session whose owner still
// believes it is alive.
func TestSweepReportsReapedSessions(t *testing.T) {
	srv := NewServer(10, WithSessionTTL(time.Nanosecond))
	now := time.Now().UTC()
	srv.mu.Lock()
	srv.sessions["s1"] = &SessionInfo{ID: "s1", User: "u@d", IP: "10.0.0.1", lastSeen: now.Add(-time.Hour)}
	srv.sessions["s2"] = &SessionInfo{ID: "s2", User: "u@d", IP: "10.0.0.1", lastSeen: now.Add(-time.Hour)}
	srv.mu.Unlock()

	before := testutil.ToFloat64(sessionsReaped)
	srv.sweepStaleSessions(now)
	if got := testutil.ToFloat64(sessionsReaped); got != before+2 {
		t.Fatalf("sessions_reaped_total = %v, want %v", got, before+2)
	}
	if got := testutil.ToFloat64(sessions); got != 0 {
		t.Fatalf("sessions gauge = %v, want 0 after sweep", got)
	}
}
