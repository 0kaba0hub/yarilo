package file

import (
	"testing"
	"time"
)

// Each rotation knob has to work on its own.
//
// It did not: the config path applied the triple only when the minimum SIZE
// was set, so an operator who set just the age saw the key in the rendered
// config, in `helm get values`, in the pod's yarilo.yaml -- and nothing
// happened. Accepted and inert is the hardest kind of setting to debug,
// because every place you look confirms it is there (#1481).
//
// A window measured on the sandbox is what found it: the age lowered from
// 300s to 60s changed nothing at all, and the same run with the minimum size
// restated -- at exactly its own default, so the only real change was that the
// guard stopped skipping -- folded once per window and took p99 from 240ms to
// 128ms.
// The fold age is a decision with a number behind it, so the number is pinned
// rather than left to whoever edits the const block next.
//
// 60s came out of the #1460 window: at 300s an actively delivering account
// carried its log to ~110 KB past the floor and paid p99 249ms against 128ms.
// Changing it back would be a product decision, and this row is where that
// decision has to be made deliberately instead of by a keystroke.
func TestTheFoldAgeDefaultIsTheMeasuredOne(t *testing.T) {
	const measured = 60 * time.Second
	if got := New().logCompactMinAge; got != measured {
		t.Errorf("built-in fold age = %s, want %s -- see #1460 before changing it", got, measured)
	}
}

func TestEachRotationArmAppliesAlone(t *testing.T) {
	tests := []struct {
		name              string
		minBytes, maxByte int64
		minAge            time.Duration
		wantMin, wantMax  int64
		wantAge           time.Duration
	}{
		{
			name:    "nothing set keeps every default",
			wantMin: defaultLogCompactMinBytes, wantMax: defaultLogCompactMaxBytes,
			wantAge: defaultLogCompactMinAge,
		},
		{
			// The case from the window: the age alone, which used to be inert.
			name:    "the age alone",
			minAge:  60 * time.Second,
			wantMin: defaultLogCompactMinBytes, wantMax: defaultLogCompactMaxBytes,
			wantAge: 60 * time.Second,
		},
		{
			name:    "the ceiling alone",
			maxByte: 64 * 1024,
			wantMin: defaultLogCompactMinBytes,
			wantMax: 64 * 1024,
			wantAge: defaultLogCompactMinAge,
		},
		{
			name:     "the floor alone",
			minBytes: 8 * 1024,
			wantMin:  8 * 1024,
			wantMax:  defaultLogCompactMaxBytes,
			wantAge:  defaultLogCompactMinAge,
		},
		{
			name:     "all three",
			minBytes: 8 * 1024, maxByte: 64 * 1024, minAge: 30 * time.Second,
			wantMin: 8 * 1024, wantMax: 64 * 1024, wantAge: 30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := New(WithLogCompaction(tc.minBytes, tc.maxByte, tc.minAge))
			if b.logCompactMinBytes != tc.wantMin {
				t.Errorf("min bytes = %d, want %d", b.logCompactMinBytes, tc.wantMin)
			}
			if b.logCompactMaxBytes != tc.wantMax {
				t.Errorf("max bytes = %d, want %d", b.logCompactMaxBytes, tc.wantMax)
			}
			if b.logCompactMinAge != tc.wantAge {
				t.Errorf("min age = %s, want %s", b.logCompactMinAge, tc.wantAge)
			}
		})
	}
}
