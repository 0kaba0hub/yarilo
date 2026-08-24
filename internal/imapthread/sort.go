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
	ordered := make([]Message, len(msgs))
	copy(ordered, msgs)

	sort.SliceStable(ordered, func(i, j int) bool {
		for _, c := range criteria {
			cmp := compareKey(c.Key, &ordered[i], &ordered[j])
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

	out := make([]uint32, len(ordered))
	for i := range ordered {
		out[i] = ordered[i].Num
	}
	return out
}

func compareKey(key imaplib.SortKey, a, b *Message) int {
	switch key {
	case imaplib.SortKeyArrival:
		return compareTime(a.Arrival, b.Arrival)
	case imaplib.SortKeyDate:
		return compareTime(a.Sent, b.Sent)
	case imaplib.SortKeySize:
		switch {
		case a.Size < b.Size:
			return -1
		case a.Size > b.Size:
			return 1
		default:
			return 0
		}
	case imaplib.SortKeySubject:
		aBase, _ := BaseSubject(a.Subject)
		bBase, _ := BaseSubject(b.Subject)
		return collate(aBase, bBase)
	case imaplib.SortKeyFrom:
		return collate(a.From, b.From)
	case imaplib.SortKeyTo:
		return collate(a.To, b.To)
	case imaplib.SortKeyCc:
		return collate(a.Cc, b.Cc)
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

// collate compares two strings the way RFC 5256 §7 requires: i;unicode-casemap
// (RFC 5051), which maps to uppercase and then compares the encoded form.
//
// Go's ToUpper is the simple uppercase mapping that collation is built on; the
// titlecase pass of RFC 5051 is not applied, so a handful of characters whose
// title and upper forms differ can collate differently from a strict
// implementation. Named rather than claimed: this is not the same as declaring
// I18NLEVEL=1, which yarilo does not advertise.
func collate(a, b string) int {
	return strings.Compare(strings.ToUpper(a), strings.ToUpper(b))
}
