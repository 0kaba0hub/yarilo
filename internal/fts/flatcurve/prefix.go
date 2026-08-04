//go:build flatcurve

package flatcurve

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PrefixRange decides which search terms are treated as prefixes.
//
// A prefix search asks the index for every term beginning with what the user
// typed, which is what makes "corp" find "corporate" — and what makes "co"
// enumerate a large part of the index. The setting exists because those are the
// same operation at different costs, and only the caller knows which it wants.
//
// Min and Max are in runes, not bytes: a two-character Cyrillic word is four
// bytes, and a threshold that counted bytes would apply a different rule to
// every script. The indexing gate still counts bytes — #1055.
type PrefixRange struct {
	// Enabled false means no term is ever expanded.
	Enabled bool
	// Min is the shortest term expanded. Zero with Enabled means every length,
	// which is what the setting spells "yes".
	Min int
	// Max is the longest term expanded; zero means no upper bound. A long term
	// is a poor candidate for expansion — the user has already been specific —
	// and the bound exists so a run of very long tokens cannot each open a
	// wildcard.
	Max int
}

// Allows reports whether a term of this length is expanded.
func (p PrefixRange) Allows(term string) bool {
	if !p.Enabled {
		return false
	}
	n := utf8.RuneCountInString(term)
	if p.Min > 0 && n < p.Min {
		return false
	}
	if p.Max > 0 && n > p.Max {
		return false
	}
	return true
}

// ParsePrefixRange reads the setting: "yes", "no", "N" or "N-M".
//
// "N" is the useful form — treat terms of at least N characters as prefixes —
// and the range is accepted because the syntax costs nothing and an upper bound
// is occasionally what an operator wants.
func ParsePrefixRange(s string) (PrefixRange, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "yes", "true":
		return PrefixRange{Enabled: true}, nil
	case "no", "false":
		return PrefixRange{}, nil
	default:
		lo, hi, isRange := strings.Cut(v, "-")
		min, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || min < 1 {
			return PrefixRange{}, fmt.Errorf(
				"fts/flatcurve: prefix search %q is not yes, no, N or N-M", s)
		}
		out := PrefixRange{Enabled: true, Min: min}
		if isRange {
			max, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil || max < min {
				return PrefixRange{}, fmt.Errorf(
					"fts/flatcurve: prefix search %q has an upper bound below its lower one", s)
			}
			out.Max = max
		}
		return out, nil
	}
}

// String renders the setting back, so a log line says what is in force rather
// than which struct fields happen to be set.
func (p PrefixRange) String() string {
	switch {
	case !p.Enabled:
		return "no"
	case p.Min == 0 && p.Max == 0:
		return "yes"
	case p.Max == 0:
		return strconv.Itoa(p.Min)
	default:
		return strconv.Itoa(p.Min) + "-" + strconv.Itoa(p.Max)
	}
}
