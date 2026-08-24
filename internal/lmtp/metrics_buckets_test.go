package lmtp

import "testing"

// Both histograms are read to answer the same kind of question -- did this
// figure move, and did it move at the median or in the tail -- so both are
// held to the resolution that question needs, inside the range each one
// actually measures.
//
// The second half of each row matters as much as the first: without asserting
// that the range is still covered, deleting every fine boundary would pass
// through an empty loop. That pair is the same one the dependency check
// learned (a filter that never fires is not a quiet system).
func TestBucketsResolveTheRangesTheyMeasure(t *testing.T) {
	tests := []struct {
		name       string
		buckets    []float64
		lo, hi     float64
		wantInside int
		why        string
	}{
		{
			name: "delivery", buckets: deliveryBuckets, lo: 0.004, hi: 0.257, wantInside: 6,
			why: "a delivery lands between 4 and 256ms; the drift question is whether the median or the tail is rising",
		},
		{
			name: "thread record", buckets: threadRecordBuckets, lo: 0.00025, hi: 0.0041, wantInside: 4,
			why: "the sidecar write measured ~1ms, and this metric exists to say whether that moves",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var inside int
			for i := 1; i < len(tc.buckets); i++ {
				prev, cur := tc.buckets[i-1], tc.buckets[i]
				if cur <= prev {
					t.Fatalf("boundary %d (%v) does not exceed its predecessor (%v)", i, cur, prev)
				}
				if cur < tc.lo || cur > tc.hi {
					continue
				}
				inside++
				if ratio := cur / prev; ratio > 2.0001 {
					t.Errorf("%v and %v differ by %.1fx: everything between them is one answer, and %s",
						prev, cur, ratio, tc.why)
				}
			}
			if inside < tc.wantInside {
				t.Fatalf("only %d boundaries fall between %v and %v, want at least %d -- the list no longer covers the range it measures",
					inside, tc.lo, tc.hi, tc.wantInside)
			}
		})
	}
}
