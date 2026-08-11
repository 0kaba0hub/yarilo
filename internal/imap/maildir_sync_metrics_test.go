package imap

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func syncCount(t *testing.T, result string) float64 {
	t.Helper()
	c, err := metricMaildirSync.GetMetricWithLabelValues(result)
	if err != nil {
		t.Fatalf("get counter %s: %v", result, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter %s: %v", result, err)
	}
	return m.GetCounter().GetValue()
}

func syncObservations(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metricMaildirSyncSeconds.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// Both outcomes have to be counted, and counted apart. The skipped half is the
// one the measurement is for: the scan is already gated on a change token, so a
// folder nobody touched should cost a stat — and if skips stay at zero under a
// workload that re-selects an unchanged folder, the gate is not reaching the
// case it exists for (#1265).
func TestMaildirSyncCountsBothOutcomes(t *testing.T) {
	s := &session{
		srv:               &Server{opts: Options{MaildirSyncOnSelect: true}},
		maildirSyncTokens: map[string]string{},
	}
	h := &nsHandle{box: &fakeSyncBox{token: "t1"}, idx: &fakeSyncIndex{}}

	scannedBefore, skippedBefore := syncCount(t, "scanned"), syncCount(t, "skipped")
	timedBefore := syncObservations(t)

	// First pass: nothing cached, so the folder is walked.
	s.reconcileFolder(h, "INBOX")
	if got := syncCount(t, "scanned"); got != scannedBefore+1 {
		t.Errorf("scanned = %v, want one more than %v", got, scannedBefore)
	}
	if got := syncCount(t, "skipped"); got != skippedBefore {
		t.Errorf("a first pass counted a skip")
	}

	// Second pass, same token: the gate holds and the walk does not happen.
	s.reconcileFolder(h, "INBOX")
	if got := syncCount(t, "skipped"); got != skippedBefore+1 {
		t.Errorf("skipped = %v, want one more than %v", got, skippedBefore)
	}
	if got := syncCount(t, "scanned"); got != scannedBefore+1 {
		t.Errorf("an unchanged folder was walked again")
	}

	// Both outcomes are timed: a skip that costs a stat is still a cost, and
	// leaving it out would make the histogram describe only the expensive half.
	if got := syncObservations(t); got != timedBefore+2 {
		t.Errorf("%d observations for two passes, want 2", got-timedBefore)
	}
}

// The doubles embed the interfaces as nil fields: a method neither of them
// implements panics rather than answering, so a reconcile path that started
// calling something else would be seen instead of silently counted.
type fakeSyncBox struct {
	mailbox.UserMailbox
	token string
}

func (f *fakeSyncBox) ProactiveScan() bool       { return true }
func (f *fakeSyncBox) SyncToken(_ string) string { return f.token }
func (f *fakeSyncBox) ReconcileIndex(_ mailbox.UserIndex, _ *mailbox.Folder) (mailbox.SyncStats, error) {
	return mailbox.SyncStats{}, nil
}

type fakeSyncIndex struct {
	mailbox.UserIndex
}

func (f *fakeSyncIndex) OpenFolder(name string, uv uint32) (*mailbox.Folder, error) {
	return &mailbox.Folder{ID: 1, Name: name, UIDValidity: uv}, nil
}
