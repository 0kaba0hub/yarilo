package quotawarn

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/quota"
)

func TestNew_EmptyBinDirIsNil(t *testing.T) {
	if New("", 10) != nil {
		t.Error("empty bin dir should yield a nil (log-only) runner")
	}
	if New("/some/dir", 0) == nil {
		t.Error("configured bin dir should yield a runner")
	}
}

func TestFire_NilRunnerSafe(t *testing.T) {
	// A nil runner must still evaluate + log crossings without panicking.
	var r *Runner
	warns := []quota.Warning{{Name: "s90", Resource: "storage", Threshold: "over", Percentage: 90, Execute: "warn"}}
	limits := quota.Limits{StorageBytes: 1000}
	r.Fire("u@x", "/home/u", warns, limits, quota.Usage{StorageBytes: 800}, quota.Usage{StorageBytes: 950})
}

func TestFire_NoCrossingNoRun(t *testing.T) {
	r := New(t.TempDir(), 5)
	// before/after both under the warn limit → nothing fires (and no bogus exec).
	warns := []quota.Warning{{Name: "s90", Resource: "storage", Threshold: "over", Percentage: 90, Execute: "definitely-not-a-real-binary"}}
	limits := quota.Limits{StorageBytes: 1000}
	r.Fire("u@x", "/home/u", warns, limits, quota.Usage{StorageBytes: 100}, quota.Usage{StorageBytes: 200})
}
