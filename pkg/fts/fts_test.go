package fts

import (
	"reflect"
	"testing"
)

func TestMergeScoresAnd(t *testing.T) {
	tests := []struct {
		name string
		dest []Score
		src  []Score
		want []Score
	}{
		{
			name: "common uid keeps higher score",
			dest: []Score{{1, 0.2}, {3, 0.9}},
			src:  []Score{{1, 0.7}, {3, 0.1}},
			want: []Score{{1, 0.7}, {3, 0.9}},
		},
		{
			name: "dest-only uids kept as-is",
			dest: []Score{{1, 0.2}, {2, 0.5}},
			src:  []Score{{2, 0.4}},
			want: []Score{{1, 0.2}, {2, 0.5}},
		},
		{
			name: "empty src leaves dest",
			dest: []Score{{5, 0.3}},
			src:  nil,
			want: []Score{{5, 0.3}},
		},
		{
			name: "empty dest stays empty",
			dest: nil,
			src:  []Score{{5, 0.3}},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeScoresAnd(tc.dest, tc.src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MergeScoresAnd = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeScoresOr(t *testing.T) {
	tests := []struct {
		name string
		dest []Score
		src  []Score
		want []Score
	}{
		{
			name: "union with higher score on overlap",
			dest: []Score{{1, 0.2}, {3, 0.9}},
			src:  []Score{{2, 0.5}, {3, 0.95}},
			want: []Score{{1, 0.2}, {2, 0.5}, {3, 0.95}},
		},
		{
			name: "disjoint interleaved",
			dest: []Score{{2, 0.1}, {4, 0.4}},
			src:  []Score{{1, 0.6}, {3, 0.3}},
			want: []Score{{1, 0.6}, {2, 0.1}, {3, 0.3}, {4, 0.4}},
		},
		{
			name: "empty dest returns src",
			dest: nil,
			src:  []Score{{7, 0.8}},
			want: []Score{{7, 0.8}},
		},
		{
			name: "both empty",
			dest: nil,
			src:  nil,
			want: []Score{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeScoresOr(tc.dest, tc.src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MergeScoresOr = %v, want %v", got, tc.want)
			}
		})
	}
}
