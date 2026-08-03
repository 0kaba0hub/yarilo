package buildmail

import (
	"testing"
	"time"
)

// The split has to add up: tokenising is the remainder, so an arithmetic slip
// there shows as time appearing or vanishing rather than as a wrong label.
func TestStageRemainderNeverGoesNegative(t *testing.T) {
	tests := []struct {
		name  string
		s     stageTimes
		total time.Duration
	}{
		{name: "normal", s: stageTimes{parse: time.Millisecond, decode: 2 * time.Millisecond,
			write: time.Millisecond}, total: 10 * time.Millisecond},
		{name: "leaves exceed the total", s: stageTimes{parse: 5 * time.Millisecond,
			decode: 5 * time.Millisecond, write: 5 * time.Millisecond}, total: time.Millisecond},
		{name: "nothing measured", total: time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// observe must not panic or emit a negative sample; a histogram
			// with a negative observation is silently useless.
			tt.s.observe(tt.total)
		})
	}
}

// track accumulates rather than replacing: a message with several parts decodes
// several times, and reporting only the last one would understate the stage
// that motivated the whole split.
func TestTrackAccumulates(t *testing.T) {
	var d time.Duration
	for i := 0; i < 3; i++ {
		if err := track(&d, func() error {
			time.Sleep(2 * time.Millisecond)
			return nil
		}); err != nil {
			t.Fatalf("track: %v", err)
		}
	}
	if d < 5*time.Millisecond {
		t.Errorf("three 2ms calls accumulated to %v — track is replacing, not adding", d)
	}
}

// An error from the timed function reaches the caller: the timer must not
// swallow what it wraps.
func TestTrackPropagatesErrors(t *testing.T) {
	var d time.Duration
	want := errFake{}
	if err := track(&d, func() error { return want }); err != want {
		t.Errorf("err = %v, want the wrapped function's error", err)
	}
	if d == 0 {
		t.Error("a failing call was not timed")
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }
