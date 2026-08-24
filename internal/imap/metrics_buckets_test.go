package imap

import "testing"

// The resolution is the point, so it is asserted rather than left to whoever
// next edits the list.
//
// A histogram answers with the boundary above the observation, so the width of
// a bucket is the width of the answer: between neighbours a factor of four
// apart, a 7ms command and a 25ms one are the same reading. The range below
// covers what IMAP commands actually cost, and inside it no answer may be
// wider than a factor of two.
func TestCommandBucketsResolveTheRangeCommandsLiveIn(t *testing.T) {
	const (
		lo = 0.001 // 1ms
		hi = 0.130 // just past the last doubled boundary
	)
	var checked int
	for i := 1; i < len(commandBuckets); i++ {
		prev, cur := commandBuckets[i-1], commandBuckets[i]
		if cur <= prev {
			t.Fatalf("bucket %d (%v) does not exceed its predecessor (%v)", i, cur, prev)
		}
		if cur < lo || cur > hi {
			continue
		}
		checked++
		if ratio := cur / prev; ratio > 2.0001 {
			t.Errorf("boundaries %v and %v differ by %.1fx: everything between them reads the same, and 2x apart is exactly what we need to tell apart",
				prev, cur, ratio)
		}
	}
	if checked < 6 {
		t.Fatalf("only %d boundaries fall in the range commands live in; the list no longer covers it", checked)
	}
}
