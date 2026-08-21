package director

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// #1365 step 0: an event kind this build does not know is dropped -- there is
// nothing else to do with it -- but never in silence. A mixed ring is exactly
// when events quietly stop happening, and a log that says nothing turns that
// into "it works sometimes".
//
// Driven through handleRingLine, the dispatcher, rather than the helper: what
// decides whether an event is known is the switch there, and a test of the
// helper alone would keep passing if a kind fell out of that list.
func TestUnknownRingEventIsCountedAndNotApplied(t *testing.T) {
	tests := []struct {
		name      string
		line      []string
		wantLabel string
	}{
		{
			// Deliberately a kind no build has ever emitted: the point is a
			// vocabulary this member has not learned. Naming a kind that is
			// merely NEW would make this row expire the moment it is added --
			// as USER-KICKED-ESC did, one step later.
			name:      "a kind from a newer build",
			line:      []string{"FUTURE-EVENT", "10.0.0.1", "9102", "1", "u@example.com"},
			wantLabel: "FUTURE-EVENT",
		},
		{
			name:      "garbage is folded into one label",
			line:      []string{"\x01\x02 not a kind at all", "x"},
			wantLabel: "malformed",
		},
		{
			name:      "an over-long kind is folded too",
			line:      []string{strings.Repeat("A", 64)},
			wantLabel: "malformed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startServer(t)
			m := srv.membership
			before := testutil.ToFloat64(ringEventUnknown.WithLabelValues(tc.wantLabel))
			membersBefore := len(m.Members())

			m.handleRingLine(tc.line, nil)

			if got := testutil.ToFloat64(ringEventUnknown.WithLabelValues(tc.wantLabel)); got != before+1 {
				t.Errorf("counter for %q = %v, want %v", tc.wantLabel, got, before+1)
			}
			// Dropped, not applied: nothing about the ring may have changed.
			if got := len(m.Members()); got != membersBefore {
				t.Errorf("an unknown event changed the member view: %d -> %d (%v)", membersBefore, got, m.Members())
			}
		})
	}
}

// A known kind must not be counted as unknown -- otherwise the metric that is
// supposed to make a mixed ring visible fires on a healthy one, and stops
// meaning anything.
func TestKnownRingEventIsNotCountedAsUnknown(t *testing.T) {
	srv, _ := startServer(t)
	m := srv.membership
	before := testutil.ToFloat64(ringEventUnknown.WithLabelValues("USER-KICKED"))

	m.handleRingLine([]string{"USER-KICKED", "10.0.0.99", "9102", "7", "u@example.com"}, nil)

	if got := testutil.ToFloat64(ringEventUnknown.WithLabelValues("USER-KICKED")); got != before {
		t.Errorf("a known kind was counted as unknown: %v -> %v", before, got)
	}
}

// The log line is throttled per kind, the counter is not: a rollout produces
// one unknown event per user action, and a line each would bury the rest of
// the ring's log exactly when it is being read.
func TestUnknownRingEventCounterIsNotThrottled(t *testing.T) {
	srv, _ := startServer(t)
	m := srv.membership
	const kind = "SOME-NEW-EVENT"
	before := testutil.ToFloat64(ringEventUnknown.WithLabelValues(kind))

	for i := 0; i < 5; i++ {
		m.handleRingLine([]string{kind, "10.0.0.1", "9102", "1", "x"}, nil)
	}

	if got := testutil.ToFloat64(ringEventUnknown.WithLabelValues(kind)); got != before+5 {
		t.Errorf("counter = %v, want %v: the throttle must apply to the log line only", got, before+5)
	}
	if _, logged := m.unknownSeen[kind]; !logged {
		t.Error("the first occurrence of a kind must be logged")
	}
}
