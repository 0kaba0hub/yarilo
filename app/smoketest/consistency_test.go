package main

import (
	"strings"
	"testing"
)

// The judge is the whole point of the area, so it is the part exercised without
// a cluster: a backend that agrees passes, and each way of disagreeing is
// refused with a message naming what disagreed (#1043, #1206).
func TestJudgeRowRefusesEachKindOfDisagreement(t *testing.T) {
	agreeing := func() (*reading, *reading) {
		l := newReading(surfIMAP).
			field("id", "M1000").
			field("subject", "Rechnung für März").
			field("size", "4096").
			set("search", []string{"M1000", "M1001"})
		r := newReading(surfJMAP).
			field("id", "M1000").
			field("subject", "Rechnung für März").
			field("size", "4096").
			set("search", []string{"M1001", "M1000"}) // order is not the fact
		return l, r
	}

	tests := []struct {
		name   string
		break_ func(right *reading)
		// wantIn is what the verdict has to name, so a reader is not sent back
		// to the cluster to find out what "mismatch" meant.
		wantIn []string
	}{
		{
			name:   "a different identity",
			break_: func(r *reading) { r.field("id", "M2000") },
			wantIn: []string{"id", "M1000", "M2000", "imap", "jmap"},
		},
		{
			name:   "a different subject",
			break_: func(r *reading) { r.field("subject", "=?utf-8?q?Rechnung?=") },
			wantIn: []string{"subject", "Rechnung für März"},
		},
		{
			name:   "a different size",
			break_: func(r *reading) { r.field("size", "4097") },
			wantIn: []string{"size", "4096", "4097"},
		},
		{
			name:   "a fact one side does not report at all",
			break_: func(r *reading) { delete(r.fields, "size") },
			wantIn: []string{"size", "reported nothing"},
		},
		{
			name:   "a search set missing a message",
			break_: func(r *reading) { r.set("search", []string{"M1000"}) },
			wantIn: []string{"search", "M1001"},
		},
		{
			name:   "a search set with one too many",
			break_: func(r *reading) { r.set("search", []string{"M1000", "M1001", "M1002"}) },
			wantIn: []string{"search", "M1002"},
		},
	}

	if l, r := agreeing(); judgeRow("identity", l, r, defaultAllowances()) != nil {
		t.Fatalf("a backend that agrees was refused: %v", judgeRow("identity", l, r, defaultAllowances()))
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, r := agreeing()
			tc.break_(r)
			err := judgeRow("identity", l, r, defaultAllowances())
			if err == nil {
				t.Fatal("the disagreement was accepted")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verdict %q does not name %q", err, want)
				}
			}
		})
	}
}

// A row with nothing on one side is a wiring error, not a pass: the judge must
// never report agreement it did not observe.
func TestJudgeRowRefusesAMissingSide(t *testing.T) {
	if err := judgeRow("identity", newReading(surfIMAP), nil, nil); err == nil {
		t.Error("comparing against nothing reported agreement")
	}
}
