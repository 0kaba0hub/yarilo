package mdboxmap

import (
	"os"
	"testing"
	"time"
)

// rotateTestMap opens a map with an explicit triple and backdates the base so
// the log reads as `age` old. The base's mtime is where the age comes from
// (see shouldRotateLocked), so this is the whole of the time control the tests
// need — no sleeping, and no clock injection.
func rotateTestMap(t *testing.T, minSize, maxSize int64, minAge, age time.Duration) *Map {
	t.Helper()
	m, err := Open(t.TempDir(), "alice@example.com", WithLogRotation(minSize, maxSize, minAge))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	backdateBase(t, m, age)
	return m
}

func backdateBase(t *testing.T, m *Map, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(m.path, at, at); err != nil {
		t.Fatalf("chtimes base: %v", err)
	}
	fi, err := os.Stat(m.path)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	m.baseInfo = fi
}

// All three edges of the triple, each asked with the other two arms held
// harmless: under the floor nothing folds however old the log is; between the
// floor and the ceiling only age decides; over the ceiling age does not get a
// vote. A single-threshold policy passes at most two of these rows.
func TestShouldRotateHonoursEveryArmOfTheTriple(t *testing.T) {
	const (
		minSize = 32 << 10
		maxSize = 1 << 20
		minAge  = 5 * time.Minute
	)

	tests := []struct {
		name    string
		logSize int64
		age     time.Duration
		want    bool
	}{
		{"under the floor, young", minSize - 1, time.Second, false},
		{"under the floor, ancient", minSize - 1, 24 * time.Hour, false},
		{"at the floor, inside the age window", minSize, minAge - time.Second, false},
		{"at the floor, past the age window", minSize, minAge + time.Second, true},
		{"below the ceiling, inside the age window", maxSize, time.Second, false},
		{"over the ceiling, young", maxSize + 1, time.Second, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := rotateTestMap(t, minSize, maxSize, minAge, tc.age)
			m.logSize = tc.logSize
			if got := m.shouldRotateLocked(); got != tc.want {
				t.Errorf("shouldRotateLocked(size=%d, age=%v) = %v, want %v",
					tc.logSize, tc.age, got, tc.want)
			}
		})
	}
}

// A configured floor of 0 turns rotation off. Asked at a size that would fold
// under any other setting, so the row cannot pass by accident.
func TestShouldRotateDisabledByZeroFloor(t *testing.T) {
	m := rotateTestMap(t, 0, 1<<20, 5*time.Minute, 24*time.Hour)
	m.logSize = 1 << 30
	if m.shouldRotateLocked() {
		t.Error("a zero floor still folded the log")
	}
}

// An unset triple leaves the package defaults rather than reading as zeros —
// which, with a zero floor meaning "disabled", would silently turn rotation off
// for every caller that does not configure it.
func TestUnsetTripleKeepsDefaults(t *testing.T) {
	m, _ := openTestMap(t)
	backdateBase(t, m, 24*time.Hour)
	m.logSize = defaultLogRotateMinSize
	if !m.shouldRotateLocked() {
		t.Error("default floor did not fold an old log at the floor")
	}
	m.logSize = defaultLogRotateMinSize - 1
	if m.shouldRotateLocked() {
		t.Error("default floor folded a log below it")
	}
}

// The hysteresis the age arm exists for: a burst of appends that keeps the log
// between floor and ceiling folds once, at the ceiling — not once per crossing
// of the floor. Under a plain size threshold this is one fold per append past
// the floor, which is what made a busy account rewrite its base repeatedly.
func TestBurstInsideTheAgeWindowFoldsOnceAtTheCeiling(t *testing.T) {
	const (
		minSize = 1 << 10
		maxSize = 8 << 10
	)
	m := rotateTestMap(t, minSize, maxSize, time.Hour, 0)

	// Appends until the log has crossed the ceiling once, counting how often a
	// plain size threshold at the floor would have folded instead.
	folds, overFloor, prev := 0, 0, int64(0)
	for i := 0; folds == 0 && i < 1000; i++ {
		if _, err := m.AppendRecords([]RecordLayout{{FileID: 1, Offset: uint32(i * 100), Size: 100}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		// The log shrinking is the fold: flushLocked rewrites the base and
		// drops the log, so logSize returns to zero.
		if m.logSize < prev {
			folds++
			// A fold stamps the base with now; keep the window closed so the
			// age arm alone can never be what allows the next one.
			backdateBase(t, m, 0)
		} else if m.logSize >= minSize {
			overFloor++
		}
		prev = m.logSize
	}

	if folds != 1 {
		t.Fatalf("%d folds, want the burst to have crossed the ceiling exactly once", folds)
	}
	if overFloor < 10 {
		t.Fatalf("only %d appends sat above the floor; the burst is too short to distinguish the policies", overFloor)
	}
	t.Logf("one fold at the ceiling against %d appends a floor-only threshold would have folded on", overFloor)
}

// The refcount journal is the map's second write path into the same log, and
// it decides on its own line of code. A triple honoured on the append path
// only would leave a refcount-heavy account folding on every delta past the
// floor — the very shape the age arm was added for.
func TestRefcountPathObeysTheSameTriple(t *testing.T) {
	m := rotateTestMap(t, 1<<10, 1<<20, time.Hour, 0)

	uids, err := m.AppendRecords([]RecordLayout{{FileID: 1, Offset: 0, Size: 100}})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	backdateBase(t, m, 0) // the append may have folded; close the window again

	folds, overFloor, prev := 0, 0, m.logSize
	for i := 0; i < 200; i++ {
		delta := int16(1)
		if i%2 == 1 {
			delta = -1
		}
		if err := m.UpdateRefcounts(uids, delta); err != nil {
			t.Fatalf("refcount %d: %v", i, err)
		}
		if m.logSize < prev {
			folds++
			backdateBase(t, m, 0)
		} else if m.logSize >= 1<<10 {
			overFloor++
		}
		prev = m.logSize
	}

	if overFloor < 10 {
		t.Fatalf("only %d deltas sat above the floor; too short to distinguish", overFloor)
	}
	if folds != 0 {
		t.Errorf("%d folds from the refcount path inside the age window, want 0", folds)
	}
}

// The same burst with the age window open folds repeatedly — the row that
// proves the previous test measured the age arm and not some incidental cap.
func TestBurstPastTheAgeWindowFoldsRepeatedly(t *testing.T) {
	m := rotateTestMap(t, 1<<10, 8<<10, time.Hour, 24*time.Hour)

	folds, prev := 0, int64(0)
	for i := 0; i < 400; i++ {
		if _, err := m.AppendRecords([]RecordLayout{{FileID: 1, Offset: uint32(i * 100), Size: 100}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if m.logSize < prev {
			folds++
			backdateBase(t, m, 24*time.Hour)
		}
		prev = m.logSize
	}

	if folds < 2 {
		t.Errorf("%d folds past the age window, want the log folded repeatedly", folds)
	}
}
