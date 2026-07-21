package imap

import "testing"

// TestRelevancyScoresNormalization (#668) verifies relevancyScores against
// the reference implementation's own SEARCH RETURN (RELEVANCY) formula: a
// plain per-result-set linear min-max scale to integers 1-100, floored at 1,
// with diff defaulting to 1.0 when every raw score in the set is equal.
func TestRelevancyScoresNormalization(t *testing.T) {
	tests := []struct {
		name  string
		raw   map[uint32]float64
		order []uint32
		want  []uint32
	}{
		{
			name:  "empty order",
			raw:   map[uint32]float64{},
			order: nil,
			want:  nil,
		},
		{
			// A single-message result set has no spread to rank against
			// (lo == hi), so diff defaults to 1.0 and the score floors to 1
			// — same as the uniform-scores case below.
			name:  "single UID floors to 1 (no spread to normalize against)",
			raw:   map[uint32]float64{1: 4.2},
			order: []uint32{1},
			want:  []uint32{1},
		},
		{
			name:  "uniform scores floor to 1",
			raw:   map[uint32]float64{1: 5.0, 2: 5.0, 3: 5.0},
			order: []uint32{1, 2, 3},
			want:  []uint32{1, 1, 1},
		},
		{
			name:  "linear spread",
			raw:   map[uint32]float64{1: 2.0, 2: 6.0, 3: 10.0},
			order: []uint32{1, 2, 3},
			want:  []uint32{1, 50, 100},
		},
		{
			name:  "order determines output order, not UID value",
			raw:   map[uint32]float64{5: 10.0, 2: 2.0},
			order: []uint32{5, 2},
			want:  []uint32{100, 1},
		},
		{
			name:  "never rounds down to 0",
			raw:   map[uint32]float64{1: 0.0, 2: 0.005, 3: 10.0},
			order: []uint32{1, 2, 3},
			want:  []uint32{1, 1, 100},
		},
		{
			// Review finding: UID 2 has no engine score at all (e.g. matched
			// only via a stripped non-FTS criterion). A plain map lookup
			// defaults it to 0.0, which — if it were allowed into the range
			// calculation — would drag lo down to 0 and compress every real
			// score toward 100. It must be excluded from lo/hi and floored
			// to 1 in the output, without altering UID 1/3's genuine scores.
			name:  "score-less UID excluded from range, floored to 1",
			raw:   map[uint32]float64{1: 8.0, 3: 10.0}, // UID 2 absent
			order: []uint32{1, 2, 3},
			want:  []uint32{1, 1, 100},
		},
		{
			// If EVERY UID lacks a score, there is no range at all — every
			// output floors to 1 rather than panicking or dividing by zero.
			name:  "every UID score-less",
			raw:   map[uint32]float64{},
			order: []uint32{1, 2, 3},
			want:  []uint32{1, 1, 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := relevancyScores(tc.raw, tc.order)
			if len(got) != len(tc.want) {
				t.Fatalf("relevancyScores() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("relevancyScores()[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}
