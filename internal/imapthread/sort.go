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
	//
	// One column per criterion, not one row per message: a row carried three
	// slices whatever the key was, so a one-key sort over 10 442 messages
	// allocated 31 326 slices to hold 10 442 values (#1487).
	cols := make([]column, len(criteria))
	for k := range criteria {
		cols[k] = newColumn(criteria[k].Key, msgs)
	}

	// Sort a permutation: a swap moves an int, not the keys themselves.
	perm := make([]int, len(msgs))
	for i := range perm {
		perm[i] = i
	}

	// Stable, so messages equal on every stated key keep their mailbox order --
	// the implicit criterion of §3, which REVERSE does not touch.
	sort.SliceStable(perm, func(x, y int) bool {
		i, j := perm[x], perm[y]
		for k := range criteria {
			cmp := cols[k].compare(i, j)
			if cmp == 0 {
				continue
			}
			if criteria[k].Reverse {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})

	out := make([]uint32, len(perm))
	for i, idx := range perm {
		out[i] = msgs[idx].Num
	}
	return out
}

// column holds one criterion's key for every message, in the form it compares
// in: strings already collated, times and sizes already extracted. Only the
// slice the key needs is allocated -- ARRIVAL never builds a string, SUBJECT
// never builds a time -- so a sort allocates per criterion, not per message.
type column struct {
	text []string
	time []time.Time
	size []int64
}

func newColumn(key imaplib.SortKey, msgs []Message) column {
	var c column
	switch key {
	case imaplib.SortKeyArrival, imaplib.SortKeyDate:
		c.time = make([]time.Time, len(msgs))
		for i := range msgs {
			if key == imaplib.SortKeyArrival {
				c.time[i] = msgs[i].Arrival
			} else {
				c.time[i] = msgs[i].Sent
			}
		}
	case imaplib.SortKeySize:
		c.size = make([]int64, len(msgs))
		for i := range msgs {
			c.size[i] = msgs[i].Size
		}
	case imaplib.SortKeySubject, imaplib.SortKeyFrom, imaplib.SortKeyTo, imaplib.SortKeyCc:
		c.text = make([]string, len(msgs))
		for i := range msgs {
			switch key {
			case imaplib.SortKeySubject:
				base, _ := BaseSubject(msgs[i].Subject)
				c.text[i] = collationKey(base)
			case imaplib.SortKeyFrom:
				c.text[i] = collationKey(msgs[i].From)
			case imaplib.SortKeyTo:
				c.text[i] = collationKey(msgs[i].To)
			case imaplib.SortKeyCc:
				c.text[i] = collationKey(msgs[i].Cc)
			}
		}
	}
	return c
}

func (c *column) compare(i, j int) int {
	switch {
	case c.time != nil:
		return compareTime(c.time[i], c.time[j])
	case c.size != nil:
		switch {
		case c.size[i] < c.size[j]:
			return -1
		case c.size[i] > c.size[j]:
			return 1
		default:
			return 0
		}
	case c.text != nil:
		return strings.Compare(c.text[i], c.text[j])
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
