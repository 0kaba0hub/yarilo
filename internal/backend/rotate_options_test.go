package backend

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// The other half of #1481: the config path has to hand the knobs on when any
// one of them is set, not only when the size is.
//
// This is the seam the sandbox window actually hit -- the option constructor
// below it was willing, but nothing ever called it.
func TestIndexOptionsForwardsAnySingleRotationKnob(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.StorageConfig
		want bool
	}{
		{name: "nothing set", want: false},
		{name: "age alone", cfg: config.StorageConfig{MailIndexLogRotateMinAge: 60}, want: true},
		{name: "ceiling alone", cfg: config.StorageConfig{MailIndexLogRotateMaxSize: 64 << 10}, want: true},
		{name: "floor alone", cfg: config.StorageConfig{MailIndexLogRotateMinSize: 8 << 10}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Some options are always present -- the locker, the on-disk name
			// encoding -- so the baseline is what an empty config produces and
			// a forwarded rotation is anything above it. Counting against a
			// fixed number made this test fail the next time an unconditional
			// option was added, which says nothing about rotation.
			baseline := len(IndexOptions(config.StorageConfig{}, nil))
			got := len(IndexOptions(tc.cfg, nil)) > baseline
			if got != tc.want {
				t.Errorf("rotation forwarded = %v, want %v -- a knob the operator set is being dropped between the config and the index",
					got, tc.want)
			}
		})
	}
}
