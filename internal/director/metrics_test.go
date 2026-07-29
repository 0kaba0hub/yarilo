package director

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestObserveLookupOutcomes pins the LOOKUP result taxonomy. `killing` and
// `no_backends` both make a login proxy retry, but only one of them is a
// healthy state — a single latency series cannot express that.
func TestObserveLookupOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		{"sticky pin honoured", "sticky"},
		{"fresh assignment", "assigned"},
		{"confirmed-kick hold", "killing"},
		{"empty ring", "no_backends"},
		{"malformed request", "bad_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.CollectAndCount(lookupSeconds)
			observeLookup(tc.result, time.Now())
			if got := testutil.CollectAndCount(lookupSeconds); got < before {
				t.Fatalf("series count shrank: %d → %d", before, got)
			}
			if got := testutil.CollectAndCount(lookupSeconds); got == 0 {
				t.Fatal("no lookup_seconds series registered")
			}
		})
	}
}
