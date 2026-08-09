//go:build flatcurve

package ftsservice

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A labelled counter with no observation is not exported at all, so a run with
// nothing skipped answers "no such metric" rather than "zero". The reasons are
// declared up front for that: an operator asking how many messages were passed
// over must be able to see the answer, and the most important answer is none.
func TestSkipCounterReadsZeroBeforeAnySkip(t *testing.T) {
	got, err := testutil.GatherAndCount(prometheus.DefaultGatherer, "fts_index_skipped_total")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got < 2 {
		t.Errorf("fts_index_skipped_total exports %d series, want one per declared reason", got)
	}

	var buf strings.Builder
	if err := testutil.CollectAndCompare(metricIndexSkipped, strings.NewReader("")); err != nil {
		// CollectAndCompare against an empty expectation fails by design; the
		// value here is the dump it produces, which must name both reasons.
		buf.WriteString(err.Error())
	}
	for _, reason := range []string{`reason="read"`, `reason="other"`} {
		if !strings.Contains(buf.String(), reason) {
			t.Errorf("the counter does not export %s before anything is skipped:\n%s", reason, buf.String())
		}
	}
}
