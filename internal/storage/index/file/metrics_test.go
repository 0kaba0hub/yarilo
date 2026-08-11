package file

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func histSum(t *testing.T, h prometheus.Histogram) (float64, uint64) {
	t.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

func histVecSum2(t *testing.T, v *prometheus.HistogramVec, labels ...string) (float64, uint64) {
	t.Helper()
	h, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

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
// The named parts are the lock, the freshness check and building the answer;
// what the total holds beyond them is the finding, and it cannot be negative.
func TestReadPartsFitInsideTheWhole(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "alice@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	whole, wholeCount := histSum(t, metricReadSeconds)
	lock, lockCount := histVecSum(t, metricReadPart, "lock")
	reload, reloadCount := histVecSum(t, metricReadPart, "reload")
	build, buildCount := histVecSum(t, metricReadPart, "build")

	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	wholeAfter, wholeCountAfter := histSum(t, metricReadSeconds)
	lockAfter, lockCountAfter := histVecSum(t, metricReadPart, "lock")
	reloadAfter, reloadCountAfter := histVecSum(t, metricReadPart, "reload")
	buildAfter, buildCountAfter := histVecSum(t, metricReadPart, "build")

	if wholeCountAfter == wholeCount {
		t.Fatal("the read was not timed at all")
	}
	for name, moved := range map[string]bool{
		"lock":   lockCountAfter > lockCount,
		"reload": reloadCountAfter > reloadCount,
		"build":  buildCountAfter > buildCount,
	} {
		if !moved {
			t.Errorf("a read did not time its %s part", name)
		}
	}
	parts := (lockAfter - lock) + (reloadAfter - reload) + (buildAfter - build)
	if total := wholeAfter - whole; parts > total {
		t.Errorf("parts sum to %.6fs inside a whole of %.6fs: the spans overlap", parts, total)
	}
}

// Every read leaves the process to take a shared lock, which is the one thing
// the reference does not do here — it takes a local fcntl. The count is what
// turns "the lock is suspected" into a number, so a read must be counted as a
// round trip and not, say, as a re-entrant hit.
func TestReadsCountTheirLockRoundTrips(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "bob@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	// No lock service is wired in unit tests, so the counters must stay still:
	// counting a round trip that did not happen would put an operator's eye on
	// a cost their deployment does not pay.
	before := sharedAcquisitions(t)
	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if got := sharedAcquisitions(t); got != before {
		t.Errorf("a read counted %v lock acquisitions with no lock service wired", got-before)
	}
}

// sharedAcquisitions totals shared-mode acquisitions across every site, which
// is what a caller cares about when asking "did this read leave the process".
func sharedAcquisitions(t *testing.T) float64 {
	t.Helper()
	var total float64
	for _, site := range []string{lockSiteOpenProbe, lockSiteFallback, lockSiteRead, lockSiteWrite} {
		total += counterVecValue(t, metricLockAcquired, "shared", site)
	}
	return total
}

func counterVecValue(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get counter %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}

// slowUnlockLocker makes the release cost something measurable. It stands in
// for what the profile of a real deployment showed: releasing the lock is a
// second round trip to the lock service, about as expensive as taking it.
type slowUnlockLocker struct {
	locks.Locker
	delay time.Duration
}

func (l slowUnlockLocker) Unlock(ctx context.Context, id string) error {
	time.Sleep(l.delay)
	return l.Locker.Unlock(ctx, id)
}

// Every trip to the lock service belongs to the lock part — the acquisition and
// the release both. The release happens in a defer as the locked span ends, so
// timing only the acquisition leaves it in the remainder, where it is a cost
// with a known name sitting in the bucket reserved for costs without one.
func TestTheLockPartCoversTheRelease(t *testing.T) {
	const releaseDelay = 40 * time.Millisecond

	dial := raceTestLockServer(t)
	locker := slowUnlockLocker{Locker: dial(), delay: releaseDelay}
	root := t.TempDir()
	home := testHome(root, "carol@example.com")
	ui := New(WithLocker(locker)).OpenUser(&mailbox.UserInfo{
		Username: "carol@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	lockBefore, _ := histVecSum(t, metricReadPart, "lock")
	wholeBefore, _ := histSum(t, metricReadSeconds)
	releaseBefore, releaseCountBefore := histVecSum2(t, metricLockRelease, "shared", lockSiteRead)

	// On its own goroutine: the lock client tracks holds per goroutine, so a
	// read issued from the one that just created the folder could take the
	// re-entrant path and never touch the lock service — measuring nothing and
	// passing. A session's reads come from their own goroutine anyway.
	done := make(chan error, 1)
	go func() {
		_, gerr := ui.GetMessages(f.ID, mailbox.SeqSet{})
		done <- gerr
	}()
	if err := <-done; err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	lockAfter, _ := histVecSum(t, metricReadPart, "lock")
	wholeAfter, _ := histSum(t, metricReadSeconds)
	releaseAfter, releaseCountAfter := histVecSum2(t, metricLockRelease, "shared", lockSiteRead)

	lockPart := lockAfter - lockBefore
	whole := wholeAfter - wholeBefore
	if lockPart < releaseDelay.Seconds() {
		t.Errorf("the lock part is %.4fs for a read whose release alone took %.4fs: the release is falling into the remainder",
			lockPart, releaseDelay.Seconds())
	}
	if lockPart > whole {
		t.Errorf("the lock part (%.4fs) exceeds the whole read (%.4fs)", lockPart, whole)
	}
	if releaseCountAfter == releaseCountBefore {
		t.Error("the release was not timed on its own")
	}
	if releaseAfter-releaseBefore < releaseDelay.Seconds() {
		t.Errorf("the release histogram recorded %.4fs for a %.4fs release", releaseAfter-releaseBefore, releaseDelay.Seconds())
	}
}

// The point of the whole exercise, as a number: a read that only answers a
// client must stop going to the lock service, and a read whose answer decides a
// write must keep going. The counter is the assertion — timing would pass on a
// fast enough lock service and prove nothing.
func TestUnlockedReadsMakeNoRoundTrip(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "dave@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "dave@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	run := func(read func() error) float64 {
		before := sharedAcquisitions(t)
		done := make(chan error, 1)
		// Own goroutine: the lock client tracks holds per goroutine, so a read
		// from the one that just wrote could take the re-entrant path and
		// measure nothing while passing.
		go func() { done <- read() }()
		if err := <-done; err != nil {
			t.Fatalf("read: %v", err)
		}
		return sharedAcquisitions(t) - before
	}

	if got := run(func() error { _, e := ui.GetMessagesUnlocked(f.ID, mailbox.SeqSet{}); return e }); got != 0 {
		t.Errorf("an unlocked read took %v lock acquisitions", got)
	}
	if got := run(func() error { _, e := ui.GetMessages(f.ID, mailbox.SeqSet{}); return e }); got == 0 {
		t.Error("a locked read took none — the two paths are the same path")
	}
}

// Without the pairing there is nothing to stand on, so the unlocked entry point
// must behave as the locked one. The weaker file keeps the stronger guarantee.
func TestAnIndexWithoutLineageStaysLocked(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "erin@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "erin@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	fs := ui.open[f.ID]
	fs.mu.Lock()
	fs.lineage = lineageHdr{} // as an index written before the extension reads
	fs.mu.Unlock()

	before := sharedAcquisitions(t)
	done := make(chan error, 1)
	go func() { _, e := ui.GetMessagesUnlocked(f.ID, mailbox.SeqSet{}); done <- e }()
	if err := <-done; err != nil {
		t.Fatalf("read: %v", err)
	}
	if sharedAcquisitions(t) == before {
		t.Error("a folder with no lineage was read without the lock")
	}
}

// Every reader that only answers a client must take zero round trips, and every
// reader whose answer decides a write must still take them. Enumerated rather
// than sampled: the classification in #1249 is the deliverable, and a table is
// how it stays checkable when someone adds a reader.
func TestReadersTakeTheLockTheirClassificationSays(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "frank@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "frank@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	tests := []struct {
		name      string
		read      func() error
		wantLocks bool
	}{
		{"messages, answering a client", func() error { _, e := ui.GetMessagesUnlocked(f.ID, mailbox.SeqSet{}); return e }, false},
		{"vanished, answering a client", func() error { _, e := ui.VanishedUnlocked(f.ID, 0); return e }, false},
		{"keywords, answering a client", func() error { _, e := ui.KeywordsUnlocked(f.ID); return e }, false},
		{"messages, deciding a write", func() error { _, e := ui.GetMessages(f.ID, mailbox.SeqSet{}); return e }, true},
		{"vanished, deciding a write", func() error { _, e := ui.Vanished(f.ID, 0); return e }, true},
		{"keywords, deciding a write", func() error { _, e := ui.Keywords(f.ID); return e }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := sharedAcquisitions(t)
			done := make(chan error, 1)
			// Own goroutine: holds are tracked per goroutine, so a read issued
			// from the writer's would take the re-entrant path and measure
			// nothing while passing.
			go func() { done <- tc.read() }()
			if err := <-done; err != nil {
				t.Fatalf("read: %v", err)
			}
			got := sharedAcquisitions(t) - before
			if tc.wantLocks && got == 0 {
				t.Error("no round trip taken by a read whose answer decides a write")
			}
			if !tc.wantLocks && got != 0 {
				t.Errorf("%v round trips taken by a read that only answers a client", got)
			}
		})
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// Each site has to be reachable from its own path, or the label answers a
// question nobody asked. The one that matters is reload-fallback against
// open-probe: the first says the migration has not reached this folder, the
// second says a folder was opened. They cost the same and mean opposite things,
// which is exactly why counting them together explained nothing.
func TestEachLockSiteIsReachedFromItsOwnPath(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "iris@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "iris@example.com", Home: home,
	}).(*userHandle).ui

	site := func(mode, s string) float64 { return counterVecValue(t, metricLockAcquired, mode, s) }

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	done := make(chan error, 1)

	// A write.
	writeBefore := site("exclusive", lockSiteWrite)
	go func() {
		done <- ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10})
	}()
	if err := <-done; err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if site("exclusive", lockSiteWrite) == writeBefore {
		t.Error("a write took no exclusive lock")
	}

	// Opening a folder this handle already has open: the probe that refreshes
	// the snapshot before handing it back.
	probeBefore := site("shared", lockSiteOpenProbe)
	go func() { _, e := ui.OpenFolder("INBOX", 42, ""); done <- e }()
	if err := <-done; err != nil {
		t.Fatalf("second open: %v", err)
	}
	if site("shared", lockSiteOpenProbe) == probeBefore {
		t.Error("re-opening a folder took no probe acquisition")
	}

	// A read that is locked on purpose.
	readBefore := site("shared", lockSiteRead)
	go func() { _, e := ui.GetMessages(f.ID, mailbox.SeqSet{}); done <- e }()
	if err := <-done; err != nil {
		t.Fatalf("locked read: %v", err)
	}
	if site("shared", lockSiteRead) == readBefore {
		t.Error("a deliberately locked read was not counted as one")
	}

	// A read that wanted the lock-free path and has nothing to prove freshness
	// with: the folder as it looks before the migration reaches it.
	fs := ui.open[f.ID]
	fs.mu.Lock()
	fs.lineage = lineageHdr{}
	fs.mu.Unlock()
	fallbackBefore := site("shared", lockSiteFallback)
	go func() { _, e := ui.GetMessagesUnlocked(f.ID, mailbox.SeqSet{}); done <- e }()
	if err := <-done; err != nil {
		t.Fatalf("fallback read: %v", err)
	}
	if site("shared", lockSiteFallback) == fallbackBefore {
		t.Error("a fallback read was not counted apart from a deliberate one")
	}
	if site("shared", lockSiteRead) != readBefore+1 {
		t.Error("the fallback was counted as a deliberately locked read")
	}
}
