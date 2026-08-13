package main

import (
	"sort"
	"strings"
	"time"
)

// defaultAllowances is the whole list of differences two surfaces are permitted
// to have. Anything not here is a defect.
//
// Deliberately short, and deliberately data. Every entry had to be argued for
// once; an entry added to make a red row green is the mechanism this area
// exists to prevent, so growth is reviewed as a claim about the protocols, not
// as test maintenance.
func defaultAllowances() []allowance {
	return []allowance{
		{
			// IMAP writes an absent value as NIL, JSON as null or as nothing
			// at all. Not field-scoped: the three spellings are not values any
			// surface reports for a fact it does have.
			name:  "NIL against null",
			equal: func(l, r string) bool { return isAbsent(l) && isAbsent(r) },
		},
		{
			// A system flag is \Seen over IMAP and $seen as a JMAP keyword;
			// RFC 8621 fixes the mapping, including the case fold.
			name:  "flag against keyword spelling",
			field: "flags",
			equal: func(l, r string) bool { return flagKey(l) == flagKey(r) },
		},
		{
			// The same instant, spelled as an IMAP INTERNALDATE and as an
			// RFC 3339 timestamp. Equality is of the instant, not of the
			// rendering: a zone offset is a rendering.
			name:  "date normalisation",
			field: "internalDate",
			equal: func(l, r string) bool {
				lt, lok := parseAnyTime(l)
				rt, rok := parseAnyTime(r)
				return lok && rok && lt.Equal(rt)
			},
		},
		{
			// One header, several addresses: the set is the fact, the order is
			// not. Applies to address fields only, where a reordering cannot
			// change meaning.
			name:  "address ordering",
			field: "addresses",
			equal: func(l, r string) bool { return sameAddressSet(l, r) },
		},
	}
}

func isAbsent(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "nil", "null":
		return true
	}
	return false
}

// flagKey folds an IMAP system flag and a JMAP keyword onto one key:
// "\Seen" and "$seen" both become "seen". A non-system flag keeps its own
// name, so a custom keyword still has to match a custom keyword.
func flagKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimLeft(s, `\$`)
}

// parseAnyTime reads the spellings the compared surfaces use: IMAP's
// INTERNALDATE and RFC 3339. A value neither layout accepts is not silently
// treated as equal to anything — the caller sees "not parsed" as "not equal".
func parseAnyTime(s string) (time.Time, bool) {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	for _, layout := range []string{
		"02-Jan-2006 15:04:05 -0700", // IMAP INTERNALDATE
		time.RFC3339,
		time.RFC1123Z,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func sameAddressSet(l, r string) bool {
	ls, rs := splitAddresses(l), splitAddresses(r)
	if len(ls) != len(rs) {
		return false
	}
	for i := range ls {
		if ls[i] != rs[i] {
			return false
		}
	}
	return true
}

func splitAddresses(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
