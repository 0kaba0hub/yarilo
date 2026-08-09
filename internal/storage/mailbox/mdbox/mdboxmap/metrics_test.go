package mdboxmap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// histSum reads one histogram's total observed seconds and sample count. The
// counters are the evidence an optimisation PR argues from, so they are worth
// asserting rather than trusting.
func histSum(t *testing.T, h prometheus.Histogram) (sum float64, count uint64) {
	t.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// slowLocker takes its time granting the lock, which is what a busy lock
// service looks like from here.
type slowLocker struct {
	delay time.Duration
	// released closes once the first holder has let go, so a test can tell a
	// blocked read from a slow one.
	mu sync.Mutex
}

func (l *slowLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	time.Sleep(l.delay)
	l.mu.Lock()
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}
func (l *slowLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}
func (l *slowLocker) Unlock(context.Context, string) error               { l.mu.Unlock(); return nil }
func (l *slowLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *slowLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return make(chan locks.Event), nil
}
func (l *slowLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *slowLocker) HoldsResource(string) bool                                   { return false }
func (l *slowLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (l *slowLocker) Close() error { return nil }

// Waiting for the lock service and working under the lock are different
// findings, so they must not land in one number: an optimisation aimed at the
// wrong one is what an undivided measurement buys.
func TestLockWaitAndHoldAreCountedApart(t *testing.T) {
	const delay = 60 * time.Millisecond
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com", WithLocker(&slowLocker{delay: delay}), WithOwner("test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	waitBefore, waitCountBefore := histSum(t, metricMapLockWait)
	holdBefore, holdCountBefore := histSum(t, metricMapLockHold)

	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	wait, waitCount := histSum(t, metricMapLockWait)
	hold, holdCount := histSum(t, metricMapLockHold)
	if waitCount != waitCountBefore+1 || holdCount != holdCountBefore+1 {
		t.Fatalf("one append produced %d waits and %d holds, want one of each",
			waitCount-waitCountBefore, holdCount-holdCountBefore)
	}
	if got := wait - waitBefore; got < delay.Seconds() {
		t.Errorf("recorded %.3fs of waiting for a lock that took %v", got, delay)
	}
	// The work itself did not sleep, so a hold as long as the wait would mean
	// the two are measuring the same span.
	if got := hold - holdBefore; got >= delay.Seconds() {
		t.Errorf("hold %.3fs includes the %v wait: the spans are not separated", got, delay)
	}
}

// The number H1 turns on: a read takes no cross-process lock, but it queues on
// the in-process mutex behind a writer that is waiting for one.
func TestReadBlockedTimeIsRecordedWhileAWriterWaits(t *testing.T) {
	const delay = 80 * time.Millisecond
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com", WithLocker(&slowLocker{delay: delay}), WithOwner("test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	before, countBefore := histSum(t, metricMapReadBlocked)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := m.AppendRecord(1, 0, 10, [16]byte{2}); err != nil {
			t.Errorf("AppendRecord: %v", err)
		}
	}()
	time.Sleep(delay / 4) // let the writer reach the lock service
	if _, _, err := m.Lookup(1); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	wg.Wait()

	blocked, count := histSum(t, metricMapReadBlocked)
	if count != countBefore+1 {
		t.Fatalf("recorded %d read waits, want 1", count-countBefore)
	}
	// The read was issued while the writer sat in the lock service, so the
	// wait it recorded is that writer's, which is the cost H1 is about.
	if got := blocked - before; got < (delay / 2).Seconds() {
		t.Errorf("read recorded %.3fs of blocking behind a writer waiting %v", got, delay)
	}
}

// Freshness checks are counted by outcome: a run where every check takes the
// fast path costs two stats, one where each replays a sibling's log does not.
func TestReloadOutcomesAreCountedByKind(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{3}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	fastBefore := counterValue(t, metricMapReload.WithLabelValues("fast"))
	if _, _, err := m.Lookup(1); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// A hit needs no freshness check at all; a miss takes one and finds
	// nothing changed.
	if _, _, err := m.Lookup(9999); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := counterValue(t, metricMapReload.WithLabelValues("fast")) - fastBefore; got < 1 {
		t.Errorf("a miss on an unchanged map recorded %v fast checks, want at least one", got)
	}

	// A sibling appends through its own handle: the next read must replay the
	// log, and the bytes it replays are the cost that check is measuring.
	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	defer sibling.Close() //nolint:errcheck
	uid, err := sibling.AppendRecord(2, 0, 10, [16]byte{4})
	if err != nil {
		t.Fatalf("sibling AppendRecord: %v", err)
	}

	replayBefore := counterValue(t, metricMapReload.WithLabelValues("replay"))
	bytesBefore := counterValue(t, metricMapReplayBytes)
	if _, ok, err := m.Lookup(uid); err != nil || !ok {
		t.Fatalf("Lookup of the sibling's record: ok=%v err=%v", ok, err)
	}
	if got := counterValue(t, metricMapReload.WithLabelValues("replay")) - replayBefore; got != 1 {
		t.Errorf("recorded %v log replays, want 1", got)
	}
	if got := counterValue(t, metricMapReplayBytes) - bytesBefore; got <= 0 {
		t.Errorf("replayed %v bytes, want the sibling's record counted", got)
	}
}

// Compaction is a periodic stall inside somebody's append, so it is counted
// separately from the append itself.
func TestFlushIsCounted(t *testing.T) {
	m, _ := openTestMap(t)
	before := counterValue(t, metricMapFlush)
	// BumpRebuildCount is one of the paths that rewrites the base index.
	if err := m.BumpRebuildCount(); err != nil {
		t.Fatalf("BumpRebuildCount: %v", err)
	}
	if got := counterValue(t, metricMapFlush) - before; got != 1 {
		t.Errorf("recorded %v flushes, want 1", got)
	}
	if _, count := histSum(t, metricMapFlushSeconds); count == 0 {
		t.Error("flush duration was not recorded")
	}
}

// Writers queue on the in-process mutex before any of them reaches the lock
// service, and that time belongs to no other counter: without it the totals
// cannot reconcile with the window they were taken over, which is the one
// thing the measurement plan asked for.
func TestWriteBlockedTimeIsRecorded(t *testing.T) {
	const delay = 80 * time.Millisecond
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com", WithLocker(&slowLocker{delay: delay}), WithOwner("test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	before, countBefore := histSum(t, metricMapWriteBlocked)

	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.AppendRecord(uint32(i+1), 0, 10, [16]byte{byte(i + 5)}); err != nil {
				t.Errorf("AppendRecord: %v", err)
			}
		}()
		time.Sleep(delay / 4) // the first writer reaches the lock service first
	}
	wg.Wait()

	blocked, count := histSum(t, metricMapWriteBlocked)
	if count != countBefore+2 {
		t.Fatalf("recorded %d write waits for 2 writes", count-countBefore)
	}
	// The second writer waited out the first one's lock round trip on the
	// mutex; a run where nobody waits records ~0 and fails here.
	if got := blocked - before; got < (delay / 2).Seconds() {
		t.Errorf("two concurrent writes recorded %.3fs of mutex waiting, want at least %v", got, delay/2)
	}
}

// Without a lock service there is no cross-process hold, so none is reported:
// an operator must not see time attributed to a lock their deployment has not
// got.
func TestHoldIsNotReportedWithoutALockService(t *testing.T) {
	m, _ := openTestMap(t) // no WithLocker
	_, countBefore := histSum(t, metricMapLockHold)
	_, writeBefore := histSum(t, metricMapWriteBlocked)

	if _, err := m.AppendRecord(1, 0, 10, [16]byte{9}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	if _, count := histSum(t, metricMapLockHold); count != countBefore {
		t.Errorf("recorded %d holds of a lock that does not exist", count-countBefore)
	}
	// The mutex is still taken, so that number is still real.
	if _, count := histSum(t, metricMapWriteBlocked); count != writeBefore+1 {
		t.Errorf("recorded %d mutex waits, want 1", count-writeBefore)
	}
}

// histVecSum reads one labelled histogram's total seconds and sample count.
func histVecSum(t *testing.T, v *prometheus.HistogramVec, label string) (float64, uint64) {
	t.Helper()
	h, err := v.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("get %s: %v", label, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %s: %v", label, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

// The parts must reconcile with the whole, or an analysis is left with a
// nameless remainder and no way to tell a fourth cost from a measurement bug.
// The three named parts are stat, replay and reindex; what the total holds
// beyond them is the finding, and it cannot be negative.
func TestReloadPartsFitInsideTheWhole(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	// A sibling appends so the next check does real work in every part.
	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	defer sibling.Close() //nolint:errcheck
	uid, err := sibling.AppendRecord(2, 0, 10, [16]byte{2})
	if err != nil {
		t.Fatalf("sibling AppendRecord: %v", err)
	}

	whole, wholeCount := histSum(t, metricMapReloadSeconds)
	stat, statCount := histVecSum(t, metricMapReloadPart, "stat")
	replay, replayCount := histVecSum(t, metricMapReloadPart, "replay")
	reindex, reindexCount := histVecSum(t, metricMapReloadPart, "reindex")

	if _, ok, err := m.Lookup(uid); err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}

	wholeAfter, wholeCountAfter := histSum(t, metricMapReloadSeconds)
	statAfter, statCountAfter := histVecSum(t, metricMapReloadPart, "stat")
	replayAfter, replayCountAfter := histVecSum(t, metricMapReloadPart, "replay")
	reindexAfter, reindexCountAfter := histVecSum(t, metricMapReloadPart, "reindex")

	if wholeCountAfter == wholeCount {
		t.Fatal("the freshness check was not timed at all")
	}
	// Every check pays the stats, so their count must move with the whole.
	if statCountAfter != statCount+(wholeCountAfter-wholeCount) {
		t.Errorf("%d checks recorded %d stat samples", wholeCountAfter-wholeCount, statCountAfter-statCount)
	}
	// Each part must be recorded where it happens: reading a sibling's tail
	// replays it and then walks every record to rebuild the UID index, and a
	// part left unattributed turns into a nameless remainder that reads as a
	// fourth cost.
	if replayCountAfter == replayCount {
		t.Error("the log was replayed and the replay was not timed")
	}
	if reindexCountAfter == reindexCount {
		t.Error("the index was rebuilt after the replay and the rebuild was not timed")
	}
	parts := (statAfter - stat) + (replayAfter - replay) + (reindexAfter - reindex)
	total := wholeAfter - whole
	if parts > total {
		t.Errorf("parts sum to %.6fs inside a whole of %.6fs: the spans overlap", parts, total)
	}
}

// A read is three named parts, and each must be recorded where it happens: a
// number that lumps them says the driver is slower without saying which step,
// which is what the comparison with maildir needs (#1205).
func TestReadPartsAreRecordedSeparately(t *testing.T) {
	ObserveReadPart("lookup", 5*time.Millisecond)
	ObserveReadPart("open", 7*time.Millisecond)
	ObserveReadPart("body", 11*time.Millisecond)

	for part, want := range map[string]float64{"lookup": 0.005, "open": 0.007, "body": 0.011} {
		sum, count := histVecSum(t, metricReadPart, part)
		if count == 0 {
			t.Errorf("%s was not recorded", part)
			continue
		}
		if sum < want {
			t.Errorf("%s recorded %.6fs, want at least %.6fs", part, sum, want)
		}
	}
}

// Opening a map replays and reindexes too, and no whole is being timed there.
// Counting those parts would let the sum exceed the total, and the unnamed
// remainder between them -- the point of the split -- would go negative and
// say nothing.
func TestPartsAreNotCountedOutsideAFreshnessCheck(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, replayBefore := histVecSum(t, metricMapReloadPart, "replay")
	_, reindexBefore := histVecSum(t, metricMapReloadPart, "reindex")
	_, wholeBefore := histSum(t, metricMapReloadSeconds)

	// Opening reads the base and rebuilds the index; that is not a check.
	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck

	_, replayAfter := histVecSum(t, metricMapReloadPart, "replay")
	_, reindexAfter := histVecSum(t, metricMapReloadPart, "reindex")
	_, wholeAfter := histSum(t, metricMapReloadSeconds)

	if wholeAfter != wholeBefore {
		t.Fatalf("opening the map timed %d freshness checks", wholeAfter-wholeBefore)
	}
	if replayAfter != replayBefore || reindexAfter != reindexBefore {
		t.Errorf("opening the map recorded %d replay and %d reindex parts with no whole to sit inside",
			replayAfter-replayBefore, reindexAfter-reindexBefore)
	}
}

// Opening the map is the cost that sat outside every counter: it is not a
// freshness check, so the parts of one deliberately skip it, and it happens
// once per handle rather than once per operation. A workload that opens a
// session per operation pays it per operation, hidden inside whichever
// command touched storage first.
func TestOpeningTheMapIsTimedWholeAndInParts(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	whole, wholeCount := histSum(t, metricMapOpenSeconds)
	base, baseCount := histVecSum(t, metricMapOpenPart, "base")
	replay, replayCount := histVecSum(t, metricMapOpenPart, "replay")
	reindex, reindexCount := histVecSum(t, metricMapOpenPart, "reindex")

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck

	wholeAfter, wholeCountAfter := histSum(t, metricMapOpenSeconds)
	if wholeCountAfter != wholeCount+1 {
		t.Fatalf("one open recorded %d timings", wholeCountAfter-wholeCount)
	}
	baseAfter, baseCountAfter := histVecSum(t, metricMapOpenPart, "base")
	replayAfter, replayCountAfter := histVecSum(t, metricMapOpenPart, "replay")
	reindexAfter, reindexCountAfter := histVecSum(t, metricMapOpenPart, "reindex")
	for name, moved := range map[string]bool{
		"base":    baseCountAfter > baseCount,
		"replay":  replayCountAfter > replayCount,
		"reindex": reindexCountAfter > reindexCount,
	} {
		if !moved {
			t.Errorf("opening the map did not time its %s part", name)
		}
	}
	parts := (baseAfter - base) + (replayAfter - replay) + (reindexAfter - reindex)
	if total := wholeAfter - whole; parts > total {
		t.Errorf("parts sum to %.6fs inside a whole of %.6fs: the spans overlap", parts, total)
	}
}
