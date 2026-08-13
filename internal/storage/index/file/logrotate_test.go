package file

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The index log rotates by the same triple as the mdbox map (#1258), so it is
// held to the same rows: each arm asked with the other two harmless, and the
// hysteresis the age arm exists for. A single-threshold policy passes at most
// two of these.
func TestShouldRotateHonoursEveryArmOfTheTriple(t *testing.T) {
	const (
		minBytes = 32 << 10
		maxBytes = 1 << 20
		minAge   = 5 * time.Minute
	)
	tests := []struct {
		name    string
		logSize int64
		age     time.Duration
		want    bool
	}{
		{"under the floor, young", minBytes - 1, time.Second, false},
		{"under the floor, ancient", minBytes - 1, 24 * time.Hour, false},
		{"at the floor, inside the age window", minBytes, minAge - time.Second, false},
		{"at the floor, past the age window", minBytes, minAge + time.Second, true},
		{"below the ceiling, inside the age window", maxBytes, time.Second, false},
		{"over the ceiling, young", maxBytes + 1, time.Second, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRotate(tc.logSize, minBytes, maxBytes, tc.age, minAge)
			if got != tc.want {
				t.Errorf("shouldRotate(size=%d, age=%v) = %v, want %v",
					tc.logSize, tc.age, got, tc.want)
			}
		})
	}
}

func TestShouldRotateDisabledByZeroFloor(t *testing.T) {
	if shouldRotate(1<<30, 0, 1<<20, 24*time.Hour, 5*time.Minute) {
		t.Error("a zero floor still folded the log")
	}
}

// The row a per-descriptor stamp fails: a session that just opened the folder
// over a base someone folded a moment ago must not fold again on its first
// write past the floor. Age comes from the base's mtime, which a fresh
// descriptor reads correctly, where a handle-local "last flush" is zero and
// reads as "old enough" — knocking the age arm out for exactly the
// reconnect-per-cycle profile it was added for.
func TestFreshDescriptorOverAFreshlyFoldedBaseDoesNotFold(t *testing.T) {
	const (
		minBytes = 32 << 10
		maxBytes = 1 << 20
		minAge   = 5 * time.Minute
	)
	dir := t.TempDir()
	base := filepath.Join(dir, "yarilo.index")
	if err := os.WriteFile(base, []byte("base"), 0o600); err != nil {
		t.Fatalf("write base: %v", err)
	}

	tests := []struct {
		name    string
		baseAge time.Duration
		want    bool
	}{
		{"base folded a moment ago", time.Second, false},
		{"base folded long ago", minAge + time.Minute, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-tc.baseAge)
			if err := os.Chtimes(base, at, at); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			// A freshly opened descriptor: nothing flushed or reloaded through
			// it yet, so it carries no stamp of its own.
			fs := &folderState{indexPath: base, logSize: minBytes}
			got := shouldRotate(fs.logSize, minBytes, maxBytes, fs.sinceLastFold(), minAge)
			if got != tc.want {
				t.Errorf("fold on first write past the floor = %v, want %v", got, tc.want)
			}
		})
	}
}

// A base that cannot be stat'd reads as infinitely old, so the folder folds on
// size alone rather than never.
func TestUnstattableBaseReadsAsInfinitelyOld(t *testing.T) {
	fs := &folderState{indexPath: filepath.Join(t.TempDir(), "absent")}
	if age := fs.sinceLastFold(); age != time.Duration(math.MaxInt64) {
		t.Errorf("age of an unstattable base = %v, want the maximum", age)
	}
}

// The hysteresis, as the write path sees it: a burst that keeps the log between
// floor and ceiling folds once, at the ceiling, not once per crossing of the
// floor. The fold restamps the age, which is what rewriting the base does.
func TestBurstInsideTheAgeWindowFoldsOnceAtTheCeiling(t *testing.T) {
	const (
		minBytes  = 1 << 10
		maxBytes  = 8 << 10
		minAge    = time.Hour
		perAppend = 100
	)

	folds, overFloor := 0, 0
	lastFold, logSize := time.Now(), int64(0)
	for i := 0; folds == 0 && i < 1000; i++ {
		logSize += perAppend
		if shouldRotate(logSize, minBytes, maxBytes, time.Since(lastFold), minAge) {
			folds++
			logSize, lastFold = 0, time.Now()
			continue
		}
		if logSize >= minBytes {
			overFloor++
		}
	}

	if folds != 1 {
		t.Fatalf("%d folds, want the burst to have crossed the ceiling exactly once", folds)
	}
	if overFloor < 10 {
		t.Fatalf("only %d appends sat above the floor; too short to distinguish the policies", overFloor)
	}
	t.Logf("one fold at the ceiling against %d appends a floor-only threshold would have folded on", overFloor)
}
