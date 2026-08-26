package imapthread

import (
	"fmt"
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

// The keys are computed once per message, and this is the row that can tell.
//
// No assertion on the ORDER can see the difference: the old code produced the
// same answer, it just produced it after extracting the base subject on every
// comparison -- O(n log n) times something that depends on one message. Over
// 10 442 messages that was ~140 000 extractions instead of 10 442, worth ~100ms
// of a 248ms sort (#1461).
//
// So the property is counted directly. n extractions for n messages: anything
// proportional to the comparisons is the defect coming back.
func TestSubjectKeysAreExtractedOncePerMessage(t *testing.T) {
	const n = 64 // log2(64) = 6, so a per-comparison implementation runs ~6x more

	msgs := make([]Message, n)
	for i := range msgs {
		msgs[i] = Message{Num: uint32(i + 1), Subject: fmt.Sprintf("Re: subject %02d", n-i)}
	}

	var calls int
	onBaseSubject = func() { calls++ }
	defer func() { onBaseSubject = nil }()

	got := Sort(msgs, []imaplib.SortCriterion{key(imaplib.SortKeySubject)})
	if len(got) != n {
		t.Fatalf("Sort returned %d messages, want %d", len(got), n)
	}
	if calls != n {
		t.Errorf("BaseSubject ran %d times for %d messages -- the key is being extracted inside the comparison, not once per message", calls, n)
	}
	// The order still has to be right; a fast wrong answer is not the point.
	if got[0] != uint32(n) {
		t.Errorf("first = %d, want %d (subject 01 sorts first)", got[0], n)
	}
}

// TestSortAllocatesPerCommandNotPerMessage guards the column layout: the sort
// must allocate a fixed number of slices for the whole command, not a set of
// them for every message. A row-per-message layout allocates ~3n and fails here.
func TestSortAllocatesPerCommandNotPerMessage(t *testing.T) {
	const n = 1000
	msgs := make([]Message, n)
	for i := range msgs {
		msgs[i] = Message{
			Num:     uint32(i + 1),
			Subject: fmt.Sprintf("Subject %d", (i*7919)%n),
			Sent:    time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Arrival: time.Date(2026, 3, 1, 0, 0, i, 0, time.UTC),
			Size:    int64(1000 + i),
		}
	}
	tests := []struct {
		name     string
		criteria []imaplib.SortCriterion
	}{
		{"DATE", []imaplib.SortCriterion{{Key: imaplib.SortKeyDate}}},
		{"SIZE", []imaplib.SortCriterion{{Key: imaplib.SortKeySize}}},
		{"SUBJECT", []imaplib.SortCriterion{{Key: imaplib.SortKeySubject}}},
		{"SUBJECT DATE", []imaplib.SortCriterion{{Key: imaplib.SortKeySubject}, {Key: imaplib.SortKeyDate}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A bound, not a count: the collation keys of a SUBJECT sort are one
			// allocation per message and are the point of the exercise. What must
			// not scale with n is the number of slices holding them.
			allocs := testing.AllocsPerRun(3, func() { Sort(msgs, tt.criteria) })
			if limit := float64(n + 16); allocs > limit {
				t.Errorf("Sort allocated %.0f times over %d messages, want at most %.0f", allocs, n, limit)
			}
		})
	}
}
