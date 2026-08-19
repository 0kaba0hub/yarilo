package locks

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A Service with no endpoints blackholes rather than refusing, so the failure
// arrives as a deadline with nothing else to recognise it by. It has to carry
// the marker, or the protocol seam above classifies the most common form of a
// lock-service restart as SERVERBUG (#1339).
//
// A cancellation must NOT carry it: that is the caller giving up -- a client
// that disconnected -- and reporting it as a service outage would turn our own
// callers' departures into failures of yarilo-locks.
func TestOnlyDeadlinesCountAsUnavailability(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantUnavailable bool
	}{
		{"an internal deadline elapsed", context.DeadlineExceeded, true},
		{"wrapped, as a caller would", fmt.Errorf("locks: %w", context.DeadlineExceeded), true},
		{"the caller cancelled", context.Canceled, false},
		{"an ordinary error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := waitFailure(tc.err)
			if errors.Is(got, ErrUnavailable) != tc.wantUnavailable {
				t.Errorf("unavailable = %v, want %v (%v)", !tc.wantUnavailable, tc.wantUnavailable, got)
			}
			// The original cause survives either way, so a caller can still
			// tell a deadline from a cancellation.
			if !errors.Is(got, tc.err) {
				t.Errorf("the cause was lost: %v", got)
			}
		})
	}
}
