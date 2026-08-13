package main

import (
	"fmt"
	"sort"
	"strings"
)

// The consistency area asserts that two protocol surfaces describe the SAME
// message the same way. Every other area judges inside one protocol: the IMAP
// checks say IMAP answered sensibly, the JMAP checks say JMAP answered
// sensibly, and nothing asks whether the two agree (#1209).
//
// The judgement lives here, apart from the reading, and is exercised against
// stubs. A comparison that only ever runs against a cluster hides its own
// reading errors until a rollout — which is how an Email/query verification
// compared against undecoded headers and stayed green (#1043, #1206).

// surface names one protocol face of the server. Used in messages and in the
// skip that names a missing side, so the strings are the operator's words.
type surface string

const (
	surfIMAP        surface = "imap"
	surfJMAP        surface = "jmap"
	surfPOP3        surface = "pop3"
	surfLMTP        surface = "lmtp"
	surfAdminAPI    surface = "admin API"
	surfManageSieve surface = "managesieve"
)

// reading is what one surface said about the subject under comparison. Fields
// hold single facts (id, subject, size); sets hold answers that are unordered
// collections (a search result). Both are strings as that surface spells them:
// normalising at read time is how a difference gets hidden before anyone can
// judge it.
type reading struct {
	surface surface
	fields  map[string]string
	sets    map[string][]string
}

func newReading(s surface) *reading {
	return &reading{surface: s, fields: map[string]string{}, sets: map[string][]string{}}
}

func (r *reading) field(name, value string) *reading {
	r.fields[name] = value
	return r
}

func (r *reading) set(name string, values []string) *reading {
	r.sets[name] = values
	return r
}

// allowance is a difference two surfaces are permitted to have about the same
// fact — a real equivalence, not a defect. The list is explicit, short and
// data: an `if` buried in a comparison is a permission nobody can review.
//
// The list growing is itself a signal. Each entry is either a genuine
// equivalence or a defect wearing a disguise, and telling those apart is manual
// work that must not be automated away.
type allowance struct {
	name string
	// field limits the allowance to one fact. Empty means any field, which is
	// only right for spellings that cannot be confused with a value (NIL/null).
	field string
	// equal reports whether the two spellings say the same thing.
	equal func(left, right string) bool
}

// judgeRow compares two readings of the same subject. Every field the left side
// reported must be present on the right and equal, either exactly or through an
// allowance; the same for sets, compared as sets rather than sequences.
//
// The verdict names the row, the field, both surfaces and both values: a
// failure that says "mismatch" sends the reader back to the cluster to find out
// what it was.
func judgeRow(row string, left, right *reading, allowed []allowance) error {
	if left == nil || right == nil {
		return fmt.Errorf("%s: nothing to compare (a side is missing)", row)
	}
	var problems []string
	for _, name := range sortedKeys(left.fields) {
		lv := left.fields[name]
		rv, ok := right.fields[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s reported %q=%q, %s reported nothing for it",
				row, left.surface, name, lv, right.surface))
			continue
		}
		if lv == rv || tolerated(name, lv, rv, allowed) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: %s says %s=%q, %s says %q",
			row, left.surface, name, lv, right.surface, rv))
	}
	for _, name := range sortedKeys(left.sets) {
		lv := left.sets[name]
		rv, ok := right.sets[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s reported the set %s, %s reported nothing for it",
				row, left.surface, name, right.surface))
			continue
		}
		if diff := setDiff(lv, rv, name, allowed); diff != "" {
			problems = append(problems, fmt.Sprintf("%s: %s %s", row, name, diff))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// tolerated reports whether an allowance permits this pair of spellings.
func tolerated(field, left, right string, allowed []allowance) bool {
	for _, a := range allowed {
		if a.field != "" && a.field != field {
			continue
		}
		if a.equal(left, right) || a.equal(right, left) {
			return true
		}
	}
	return false
}

// setDiff describes how two sets disagree, or "" when they agree. Membership is
// judged element by element through the same allowances, so a set of dates or
// of flags is comparable without the caller normalising it first.
func setDiff(left, right []string, field string, allowed []allowance) string {
	missing := notCovered(left, right, field, allowed)
	extra := notCovered(right, left, field, allowed)
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("only on the first side: %s", strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		parts = append(parts, fmt.Sprintf("only on the second: %s", strings.Join(extra, ", ")))
	}
	return strings.Join(parts, "; ")
}

func notCovered(want, have []string, field string, allowed []allowance) []string {
	var out []string
	for _, w := range want {
		found := false
		for _, h := range have {
			if w == h || tolerated(field, w, h, allowed) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
