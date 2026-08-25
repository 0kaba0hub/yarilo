package imapthread

import (
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
)

// Sort orders the searched messages by the SORT criteria of RFC 5256 §3.
//
// msgs must arrive in mailbox order, because the specification ends every
// comparison with an implicit sequence-number criterion: messages that match
// exactly on every stated key keep the order they have in the mailbox. That is
// why the sort is stable, and why REVERSE does not touch it -- REVERSE
// reverses its own key only, so a REVERSE SUBJECT sort is not the reverse of a
// SUBJECT sort.
func Sort(msgs []Message, criteria []imaplib.SortCriterion) []uint32 {
	// Keys are computed once per message, not inside the comparison.
	//
	// They used to be computed inside it, which is O(n log n) extractions of
	// something that depends only on the message: over 10 442 messages a
	// SUBJECT sort ran BaseSubject about 140 000 times instead of 10 442, and
	// collate uppercased both sides on every one of those. Measured cost of
	// that waste: ~100ms of a 248ms SORT (SUBJECT), and the same again inside
	// every THREAD, which sorts by subject too (#1461).
	rows := make([]sortRow, len(msgs))
	for i := range msgs {
		rows[i] = newSortRow(&msgs[i], criteria)
	}

	// Stable, so messages equal on every stated key keep their mailbox order --
	// the implicit criterion of §3, which REVERSE does not touch.
	sort.SliceStable(rows, func(i, j int) bool {
		for k, c := range criteria {
			cmp := rows[i].compare(&rows[j], k, c.Key)
			if cmp == 0 {
				continue
			}
			if c.Reverse {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})

	out := make([]uint32, len(rows))
	for i := range rows {
		out[i] = rows[i].num
	}
	return out
}

// sortRow is one message reduced to what the requested keys compare, in the
// form they compare in: strings already collated, times and sizes already
// extracted. One entry per criterion, so a two-key sort computes two.
type sortRow struct {
	num  uint32
	text []string    // collated string key per criterion, "" where not a string key
	time []time.Time // per criterion
	size []int64     // per criterion
}

func newSortRow(m *Message, criteria []imaplib.SortCriterion) sortRow {
	r := sortRow{
		num:  m.Num,
		text: make([]string, len(criteria)),
		time: make([]time.Time, len(criteria)),
		size: make([]int64, len(criteria)),
	}
	for i, c := range criteria {
		switch c.Key {
		case imaplib.SortKeyArrival:
			r.time[i] = m.Arrival
		case imaplib.SortKeyDate:
			r.time[i] = m.Sent
		case imaplib.SortKeySize:
			r.size[i] = m.Size
		case imaplib.SortKeySubject:
			base, _ := BaseSubject(m.Subject)
			r.text[i] = collationKey(base)
		case imaplib.SortKeyFrom:
			r.text[i] = collationKey(m.From)
		case imaplib.SortKeyTo:
			r.text[i] = collationKey(m.To)
		case imaplib.SortKeyCc:
			r.text[i] = collationKey(m.Cc)
		}
	}
	return r
}

func (r *sortRow) compare(other *sortRow, i int, key imaplib.SortKey) int {
	switch key {
	case imaplib.SortKeyArrival, imaplib.SortKeyDate:
		return compareTime(r.time[i], other.time[i])
	case imaplib.SortKeySize:
		switch {
		case r.size[i] < other.size[i]:
			return -1
		case r.size[i] > other.size[i]:
			return 1
		default:
			return 0
		}
	case imaplib.SortKeySubject, imaplib.SortKeyFrom, imaplib.SortKeyTo, imaplib.SortKeyCc:
		return strings.Compare(r.text[i], other.text[i])
	default:
		return 0
	}
}

func compareTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case b.Before(a):
		return 1
	default:
		return 0
	}
}

// collationKey maps a string to the form RFC 5256 §7 compares: i;unicode-casemap
// (RFC 5051), which uppercases and then compares the encoded bytes. Computed
// once per message rather than on every comparison.
//
// Go's ToUpper is the simple uppercase mapping that collation is built on; the
// titlecase pass of RFC 5051 is not applied, so a handful of characters whose
// title and upper forms differ can collate differently from a strict
// implementation. Named rather than claimed: this is not the same as declaring
// I18NLEVEL=1, which yarilo does not advertise.
func collationKey(s string) string {
	return strings.ToUpper(s)
}
