package main

import (
	"testing"
)

// The permitted-difference list is exercised in BOTH directions: a difference
// on the list is tolerated, and the same difference with its entry removed is
// refused. Only the pair pins anything — a one-directional test passes just as
// well against a judge that tolerates everything.
func TestAllowedDifferencesAreToleratedAndOtherwiseRefused(t *testing.T) {
	tests := []struct {
		name      string
		allowance string // the entry that has to be what makes it pass
		field     string
		left      string
		right     string
	}{
		{"NIL against null", "NIL against null", "replyTo", "NIL", "null"},
		{"NIL against an absent value", "NIL against null", "replyTo", "NIL", ""},
		{"system flag against keyword", "flag against keyword spelling", "flags", `\Seen`, "$seen"},
		{"flag case is a spelling", "flag against keyword spelling", "flags", `\Flagged`, "$FLAGGED"},
		{"internal date rendering", "date normalisation", "internalDate",
			"14-Aug-2026 09:30:00 +0300", "2026-08-14T06:30:00Z"},
		{"encoded-word against decoded text", "encoded-word against decoded text", "subject",
			"=?utf-8?Q?Rechnung_f=C3=BCr_M=C3=A4rz_=E2=82=AC42?=", "Rechnung für März €42"},
		{"address ordering", "address ordering", "addresses",
			"a@x.test, b@x.test", "b@x.test, a@x.test"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left := newReading(surfIMAP).field(tc.field, tc.left)
			right := newReading(surfJMAP).field(tc.field, tc.right)

			if err := judgeRow("row", left, right, defaultAllowances()); err != nil {
				t.Errorf("a permitted difference was refused: %v", err)
			}
			if err := judgeRow("row", left, right, without(tc.allowance)); err == nil {
				t.Errorf("with %q removed from the list, the difference was still tolerated — "+
					"something else is making this pass and the entry pins nothing", tc.allowance)
			}
		})
	}
}

// Values that merely look alike are not on any list. Without these rows an
// allowance written too widely (a date parser that returns the zero time for
// anything, a flag fold that strips every character) would be invisible.
func TestDifferencesNotOnTheListAreRefused(t *testing.T) {
	tests := []struct {
		name  string
		field string
		left  string
		right string
	}{
		{"a real absent value against a real one", "replyTo", "NIL", "a@x.test"},
		{"two different custom keywords", "flags", "$important", "$later"},
		{"a custom keyword against a system flag", "flags", `\Seen`, "$important"},
		{"two different instants", "internalDate",
			"14-Aug-2026 09:30:00 +0300", "2026-08-14T09:30:00Z"},
		{"an unparseable date against a real one", "internalDate", "someday", "2026-08-14T06:30:00Z"},
		// The distinguishing input: a decoder and a pass-through answer the
		// same on ASCII, and differently here.
		{"an encoded-word decoding to a different subject", "subject",
			"=?utf-8?Q?Rechnung_f=C3=BCr_April?=", "Rechnung für März €42"},
		{"an undecodable encoded-word against text", "subject",
			"=?utf-8?X?bogus?=", "Rechnung für März €42"},
		{"a different address in the same count", "addresses",
			"a@x.test, b@x.test", "a@x.test, c@x.test"},
		{"the same address twice against two", "addresses",
			"a@x.test, b@x.test", "a@x.test, a@x.test"},
		// The date allowance is field-scoped: the same two spellings compared
		// as a subject are two different subjects.
		{"a date spelling outside its field", "subject",
			"14-Aug-2026 09:30:00 +0300", "2026-08-14T06:30:00Z"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left := newReading(surfIMAP).field(tc.field, tc.left)
			right := newReading(surfJMAP).field(tc.field, tc.right)
			if err := judgeRow("row", left, right, defaultAllowances()); err == nil {
				t.Errorf("%q against %q was tolerated", tc.left, tc.right)
			}
		})
	}
}

// Sets are judged through the same allowances, element by element: a set of
// flags compared across surfaces would otherwise need the caller to normalise
// it first, which is where a difference gets hidden.
func TestSetsUseTheSameAllowances(t *testing.T) {
	left := newReading(surfIMAP).set("flags", []string{`\Seen`, `\Flagged`})
	right := newReading(surfJMAP).set("flags", []string{"$flagged", "$seen"})
	if err := judgeRow("flags", left, right, defaultAllowances()); err != nil {
		t.Errorf("flag sets that agree were refused: %v", err)
	}

	right.set("flags", []string{"$flagged", "$draft"})
	if err := judgeRow("flags", left, right, defaultAllowances()); err == nil {
		t.Error("a set carrying a different flag was tolerated")
	}
}

// without returns the list minus one entry, which is how each row proves that
// the entry — and not some other leniency — is what tolerates its difference.
func without(name string) []allowance {
	var out []allowance
	for _, a := range defaultAllowances() {
		if a.name != name {
			out = append(out, a)
		}
	}
	return out
}
