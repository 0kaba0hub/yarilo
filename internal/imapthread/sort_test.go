package imapthread

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

func at(sec int) time.Time { return time.Date(2026, 3, 1, 0, 0, sec, 0, time.UTC) }

func key(k imaplib.SortKey) imaplib.SortCriterion {
	return imaplib.SortCriterion{Key: k}
}

func reverse(k imaplib.SortKey) imaplib.SortCriterion {
	return imaplib.SortCriterion{Key: k, Reverse: true}
}

func TestSortOrdersByTheStatedKeys(t *testing.T) {
	// Built so that no two keys agree: each row below is answered by exactly
	// one key, and a comparator that consulted the wrong one would show.
	msgs := []Message{
		{Num: 1, Subject: "Charlie", Sent: at(30), Arrival: at(10), Size: 300, From: "zeta", To: "alpha"},
		{Num: 2, Subject: "Alpha", Sent: at(20), Arrival: at(20), Size: 100, From: "yankee", To: "bravo"},
		{Num: 3, Subject: "Bravo", Sent: at(10), Arrival: at(30), Size: 200, From: "xray", To: "charlie"},
	}

	tests := []struct {
		name     string
		criteria []imaplib.SortCriterion
		want     []uint32
	}{
		{"subject", []imaplib.SortCriterion{key(imaplib.SortKeySubject)}, []uint32{2, 3, 1}},
		{"sent date", []imaplib.SortCriterion{key(imaplib.SortKeyDate)}, []uint32{3, 2, 1}},
		{"arrival", []imaplib.SortCriterion{key(imaplib.SortKeyArrival)}, []uint32{1, 2, 3}},
		{"size", []imaplib.SortCriterion{key(imaplib.SortKeySize)}, []uint32{2, 3, 1}},
		{"from", []imaplib.SortCriterion{key(imaplib.SortKeyFrom)}, []uint32{3, 2, 1}},
		{"to", []imaplib.SortCriterion{key(imaplib.SortKeyTo)}, []uint32{1, 2, 3}},
		{"reversed", []imaplib.SortCriterion{reverse(imaplib.SortKeySize)}, []uint32{1, 3, 2}},
		{
			// §3's own example of priority: subject first, and among equal
			// subjects the date decides. Nothing here has an equal subject, so
			// the second key must not disturb the first.
			name:     "priority follows the order given",
			criteria: []imaplib.SortCriterion{key(imaplib.SortKeySubject), key(imaplib.SortKeyDate)},
			want:     []uint32{2, 3, 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sort(msgs, tc.criteria); !equal(got, tc.want) {
				t.Errorf("Sort(%v) = %v, want %v", tc.criteria, got, tc.want)
			}
		})
	}
}

// The implicit criterion of §3: messages equal on every stated key keep their
// mailbox order. REVERSE does not touch it, which is why REVERSE SUBJECT is
// not the reverse of a SUBJECT sort -- the specification says so, and the
// difference is visible only when subjects tie.
func TestReverseDoesNotReverseTheImplicitOrder(t *testing.T) {
	msgs := []Message{
		{Num: 1, Subject: "Alpha", Sent: at(10)},
		{Num: 2, Subject: "Bravo", Sent: at(20)},
		{Num: 3, Subject: "Alpha", Sent: at(30)},
	}

	forward := Sort(msgs, []imaplib.SortCriterion{key(imaplib.SortKeySubject)})
	if !equal(forward, []uint32{1, 3, 2}) {
		t.Fatalf("SUBJECT = %v, want 1 3 2", forward)
	}
	got := Sort(msgs, []imaplib.SortCriterion{reverse(imaplib.SortKeySubject)})
	if !equal(got, []uint32{2, 1, 3}) {
		t.Errorf("REVERSE SUBJECT = %v, want 2 1 3 -- the tied pair keeps mailbox order", got)
	}
	if equal(got, []uint32{2, 3, 1}) {
		t.Error("REVERSE SUBJECT returned the reverse of the SUBJECT order, which reverses the implicit criterion too")
	}
}

// SUBJECT sorts by base subject, through the same extraction THREAD uses: a
// reply sorts with what it answers, not under "R".
func TestSubjectSortsByTheBaseSubject(t *testing.T) {
	msgs := []Message{
		{Num: 1, Subject: "Re: Zulu"},
		{Num: 2, Subject: "Alpha"},
	}
	if got := Sort(msgs, []imaplib.SortCriterion{key(imaplib.SortKeySubject)}); !equal(got, []uint32{2, 1}) {
		t.Errorf("SUBJECT = %v, want the base subjects compared (Alpha before Zulu)", got)
	}
}

// An absent header is the empty string and collates first (§3).
func TestAbsentHeadersCollateFirst(t *testing.T) {
	msgs := []Message{
		{Num: 1, From: "alpha"},
		{Num: 2, From: ""},
	}
	if got := Sort(msgs, []imaplib.SortCriterion{key(imaplib.SortKeyFrom)}); !equal(got, []uint32{2, 1}) {
		t.Errorf("FROM = %v, want the message with no From first", got)
	}
}

// Collation is case-insensitive (§7, i;unicode-casemap), so case cannot decide
// an order that the letters do not.
func TestCollationIgnoresCase(t *testing.T) {
	msgs := []Message{
		{Num: 1, Subject: "beta"},
		{Num: 2, Subject: "ALPHA"},
	}
	if got := Sort(msgs, []imaplib.SortCriterion{key(imaplib.SortKeySubject)}); !equal(got, []uint32{2, 1}) {
		t.Errorf("SUBJECT = %v, want ALPHA before beta", got)
	}
}

func equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
