package imapthread

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// TestReferencesGrowsLinearly guards the cost of re-parenting a generation.
//
// The measure is bytes, not allocations: the defect it guards against copied
// the root's whole child list on every detachment, which is one allocation of
// a growing size, so the allocation count barely moved while the bytes went
// quadratic -- 334 MB for ten thousand messages (#1461).
//
// The corpus is the one that provokes it: every subject appears twice, so step
// 5 re-parents half the top level.
func TestReferencesGrowsLinearly(t *testing.T) {
	measure := func(n int) float64 {
		msgs := subjectPairs(n)
		r := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				References(msgs)
			}
		})
		return float64(r.AllocedBytesPerOp())
	}
	small, large := measure(1000), measure(4000)
	// Four times the messages: linear is ~4x the bytes, quadratic is ~16x.
	// The bound is loose enough that a constant factor does not trip it and
	// tight enough that quadratic cannot hide under it.
	if ratio := large / small; ratio > 8 {
		t.Errorf("four times the messages cost %.1fx the bytes (%.0f -> %.0f), want at most 8x",
			ratio, small, large)
	}
}

// TestTreeStaysConsistent guards the deferred detach: takeFrom leaves a child
// in its old parent's list until compactChildren runs, so a missed compaction
// shows up as a container that two parents claim, or one whose parent does not
// hold it. Both are invisible in the output of a corpus that happens not to
// reach the stale entry, which is why this walks the tree instead of comparing
// thread strings.
func TestTreeStaysConsistent(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
	}{
		{"singletons", singletons(500)},
		{"subject pairs", subjectPairs(500)},
		{"chained", corpus(500)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := References(tt.msgs)
			seen := make(map[uint32]bool, len(tt.msgs))
			var walk func([]imaplib.ThreadNode)
			walk = func(ns []imaplib.ThreadNode) {
				for _, n := range ns {
					if n.Num != 0 {
						if seen[n.Num] {
							t.Errorf("message %d appears in the tree twice", n.Num)
						}
						seen[n.Num] = true
					}
					walk(n.Children)
				}
			}
			walk(nodes)
			for i := range tt.msgs {
				if !seen[tt.msgs[i].Num] {
					t.Errorf("message %d is missing from the tree", tt.msgs[i].Num)
				}
			}
		})
	}
}
