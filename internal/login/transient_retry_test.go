package login

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestTransientRetriesDefault pins the budget semantics, including the negative
// opt-out — an operator disabling retries must get fail-on-first-error back
// rather than the default silently applying.
func TestTransientRetriesDefault(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"zero selects the default", 0, defaultTransientRetries},
		{"explicit budget", 5, 5},
		{"one", 1, 1},
		{"negative opts out", -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{opts: Options{TransientRetries: tc.configured}}
			if got := s.transientRetries(); got != tc.want {
				t.Fatalf("transientRetries() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTransientCountersAreSeparate covers the pair of counters that make the
// retry budget observable: retries alone only say a dependency is flapping,
// exhausted says a client actually saw the failure.
func TestTransientCountersAreSeparate(t *testing.T) {
	s := &Server{opts: Options{Protocol: ProtocolIMAP}}

	stages := []string{stageAuthDial, stageAuth, stageBackendSession}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			beforeRetry := testutil.ToFloat64(transientRetries.WithLabelValues("imap", stage))
			beforeExh := testutil.ToFloat64(transientExhausted.WithLabelValues("imap", stage))

			s.incTransientRetry(stage)
			if got := testutil.ToFloat64(transientRetries.WithLabelValues("imap", stage)); got != beforeRetry+1 {
				t.Fatalf("retries = %v, want %v", got, beforeRetry+1)
			}
			// A retry must not be counted as an exhaustion.
			if got := testutil.ToFloat64(transientExhausted.WithLabelValues("imap", stage)); got != beforeExh {
				t.Fatalf("exhausted moved to %v on a retry, want %v", got, beforeExh)
			}

			s.incTransientExhausted(stage)
			if got := testutil.ToFloat64(transientExhausted.WithLabelValues("imap", stage)); got != beforeExh+1 {
				t.Fatalf("exhausted = %v, want %v", got, beforeExh+1)
			}
		})
	}
}

func TestTransientStageLabelsAreStable(t *testing.T) {
	// Dashboards key on these exact strings.
	tests := []struct {
		got, want string
	}{
		{stageAuthDial, "auth_dial"},
		{stageAuth, "auth"},
		{stageBackendSession, "backend_session"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Fatalf("stage label = %q, want %q", tc.got, tc.want)
		}
	}
}
