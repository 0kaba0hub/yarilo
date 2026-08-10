package file

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

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
	before := counterVecValue(t, metricLockAcquired, "shared")
	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if got := counterVecValue(t, metricLockAcquired, "shared"); got != before {
		t.Errorf("a read counted %v lock acquisitions with no lock service wired", got-before)
	}
}

func counterVecValue(t *testing.T, v *prometheus.CounterVec, label string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("get counter %s: %v", label, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter %s: %v", label, err)
	}
	return m.GetCounter().GetValue()
}
