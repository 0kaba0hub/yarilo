package file

import (
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
	now := time.Now()

	tests := []struct {
		name      string
		logSize   int64
		lastFlush time.Time
		want      bool
	}{
		{"under the floor, young", minBytes - 1, now, false},
		{"under the floor, ancient", minBytes - 1, now.Add(-24 * time.Hour), false},
		{"at the floor, inside the age window", minBytes, now.Add(-minAge + time.Second), false},
		{"at the floor, past the age window", minBytes, now.Add(-minAge - time.Second), true},
		{"below the ceiling, inside the age window", maxBytes, now, false},
		{"over the ceiling, young", maxBytes + 1, now, true},
		{"never flushed in this process", minBytes, time.Time{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRotate(tc.logSize, minBytes, maxBytes, tc.lastFlush, minAge)
			if got != tc.want {
				t.Errorf("shouldRotate(size=%d, lastFlush=%v) = %v, want %v",
					tc.logSize, tc.lastFlush, got, tc.want)
			}
		})
	}
}

func TestShouldRotateDisabledByZeroFloor(t *testing.T) {
	if shouldRotate(1<<30, 0, 1<<20, time.Time{}, 5*time.Minute) {
		t.Error("a zero floor still folded the log")
	}
}

// The hysteresis, as the write path sees it: a burst that keeps the log between
// floor and ceiling folds once, at the ceiling, not once per crossing of the
// floor. lastFlush is restamped at each fold, which is what the real flush does.
func TestBurstInsideTheAgeWindowFoldsOnceAtTheCeiling(t *testing.T) {
	const (
		minBytes  = 1 << 10
		maxBytes  = 8 << 10
		minAge    = time.Hour
		perAppend = 100
	)

	folds, overFloor := 0, 0
	lastFlush, logSize := time.Now(), int64(0)
	for i := 0; folds == 0 && i < 1000; i++ {
		logSize += perAppend
		if shouldRotate(logSize, minBytes, maxBytes, lastFlush, minAge) {
			folds++
			logSize, lastFlush = 0, time.Now()
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
