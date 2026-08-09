package mdbox

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherHist reads one labelled histogram's total seconds and sample count
// from the default registry, which is where the shared save metrics live.
func gatherHist(t *testing.T, name string, labels map[string]string) (float64, uint64) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	var count uint64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !hasLabels(m, labels) {
				continue
			}
			sum += m.GetHistogram().GetSampleSum()
			count += m.GetHistogram().GetSampleCount()
		}
	}
	return sum, count
}

func hasLabels(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, l := range m.GetLabel() {
			if l.GetName() == k && l.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// The named steps of a save must fit inside the save: what is left over is a
// cost nobody has named, and that remainder is the finding. A part timed
// outside the whole would make it negative, and a negative remainder says
// nothing at all.
func TestSavePartsFitInsideTheWhole(t *testing.T) {
	u, _ := newTestUser(t)

	wholeBefore, wholeCountBefore := gatherHist(t, "mailbox_save_seconds", map[string]string{"driver": "mdbox"})
	parts := []string{"read", "prepare", "open", "write", "close", "map"}
	before := map[string]float64{}
	counts := map[string]uint64{}
	for _, p := range parts {
		before[p], counts[p] = gatherHist(t, "mailbox_save_part_seconds", map[string]string{"driver": "mdbox", "part": p})
	}

	if _, _, _, err := u.Save("INBOX", strings.NewReader("Subject: t\r\n\r\nbody\r\n"), 1, 0, nil, [16]byte{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	whole, wholeCount := gatherHist(t, "mailbox_save_seconds", map[string]string{"driver": "mdbox"})
	if wholeCount != wholeCountBefore+1 {
		t.Fatalf("one save recorded %d whole timings", wholeCount-wholeCountBefore)
	}
	var partsSum float64
	for _, p := range parts {
		sum, count := gatherHist(t, "mailbox_save_part_seconds", map[string]string{"driver": "mdbox", "part": p})
		if count == counts[p] {
			t.Errorf("part %q was not timed", p)
		}
		partsSum += sum - before[p]
	}
	if total := whole - wholeBefore; partsSum > total {
		t.Errorf("parts sum to %.6fs inside a whole of %.6fs: the spans overlap", partsSum, total)
	}
}

// The comparison is the point: a save on one driver means nothing without the
// same number from the others, so all three report under one metric name.
func TestEveryDriverReportsItsSave(t *testing.T) {
	u, _ := newTestUser(t)
	if _, _, _, err := u.Save("INBOX", strings.NewReader("Subject: t\r\n\r\nbody\r\n"), 1, 0, nil, [16]byte{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, count := gatherHist(t, "mailbox_save_seconds", map[string]string{"driver": "mdbox"}); count == 0 {
		t.Error("mdbox does not report its saves under the shared metric")
	}
}
