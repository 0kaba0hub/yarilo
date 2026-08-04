//go:build flatcurve

package flatcurve

import "testing"

// The setting decides how much of the index a one-word search touches, so each
// spelling is pinned rather than sampled.
func TestParsePrefixRange(t *testing.T) {
	for _, tc := range []struct {
		in      string
		enabled bool
		min     int
		max     int
		bad     bool
	}{
		{in: "", enabled: true},
		{in: "yes", enabled: true},
		{in: "YES", enabled: true},
		{in: "true", enabled: true},
		{in: "no"},
		{in: "false"},
		{in: "3", enabled: true, min: 3},
		{in: " 3 ", enabled: true, min: 3},
		{in: "3-10", enabled: true, min: 3, max: 10},
		{in: "1-1", enabled: true, min: 1, max: 1},

		// Refused rather than read as the nearest sensible thing: a setting
		// that quietly became something else would change how much of the
		// index every search touches, and nothing downstream could tell.
		{in: "0", bad: true},
		{in: "-1", bad: true},
		{in: "10-3", bad: true},
		{in: "3-", bad: true},
		{in: "maybe", bad: true},
		{in: "3-10-20", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePrefixRange(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("%q was accepted as %+v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got.Enabled != tc.enabled || got.Min != tc.min || got.Max != tc.max {
				t.Errorf("= %+v, want enabled=%v min=%d max=%d", got, tc.enabled, tc.min, tc.max)
			}
		})
	}
}

// Lengths are counted in runes. A threshold that counted bytes would apply a
// different rule to every script — a three-letter Cyrillic word is six bytes,
// so a minimum of four would expand it while refusing a four-letter Latin one.
func TestPrefixRangeCountsRunes(t *testing.T) {
	p := PrefixRange{Enabled: true, Min: 4}
	for _, tc := range []struct {
		term  string
		allow bool
	}{
		{"abc", false}, // 3 runes, 3 bytes
		{"abcd", true}, // 4 runes, 4 bytes
		{"мова", true}, // 4 runes, 8 bytes
		{"дім", false}, // 3 runes, 6 bytes — byte counting would allow it
		{"日本語", false}, // 3 runes, 9 bytes
	} {
		if got := p.Allows(tc.term); got != tc.allow {
			t.Errorf("Allows(%q) = %v, want %v (%d runes)", tc.term, got, tc.allow, len([]rune(tc.term)))
		}
	}
}

func TestPrefixRangeBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     PrefixRange
		term  string
		allow bool
	}{
		{"disabled refuses everything", PrefixRange{}, "anything", false},
		{"yes allows a single character", PrefixRange{Enabled: true}, "a", true},
		{"below the minimum", PrefixRange{Enabled: true, Min: 3}, "ab", false},
		{"at the minimum", PrefixRange{Enabled: true, Min: 3}, "abc", true},
		{"above the maximum", PrefixRange{Enabled: true, Min: 3, Max: 5}, "abcdef", false},
		{"at the maximum", PrefixRange{Enabled: true, Min: 3, Max: 5}, "abcde", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Allows(tc.term); got != tc.allow {
				t.Errorf("Allows(%q) = %v, want %v", tc.term, got, tc.allow)
			}
		})
	}
}

// The rendered form is what a log line shows, so it has to round-trip.
func TestPrefixRangeString(t *testing.T) {
	for _, in := range []string{"yes", "no", "3", "3-10"} {
		p, err := ParsePrefixRange(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := p.String(); got != in {
			t.Errorf("ParsePrefixRange(%q).String() = %q", in, got)
		}
	}
}

// The setting has to reach the query, which is the half that was missing: the
// old code took a threshold and never read it, so every term expanded whatever
// the configuration said.
func TestPrefixSettingReachesTheQuery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		term    string
		found   bool
	}{
		{"expand every term", "yes", "butterf", true},
		{"expansion off", "no", "butterf", false},
		{"below the minimum", "8", "butterf", false},
		{"at the minimum", "7", "butterf", true},
		{"exact term still matches with expansion off", "no", "butterfly", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, _ := testEngine(t, Options{PrefixSearch: tc.setting})
			indexDoc(t, ui, 1, nil, []string{"butterfly"})

			res, err := ui.Lookup(inbox, bodyQuery(tc.term))
			if err != nil {
				t.Fatal(err)
			}
			if found := len(res.Definite) > 0; found != tc.found {
				t.Errorf("%q with prefix_search=%q found=%v, want %v",
					tc.term, tc.setting, found, tc.found)
			}
		})
	}
}

// Substring indexing writes suffixes that only prefix expansion can reach, so
// the combination cannot work. It is corrected loudly at startup rather than
// serving an index nothing can query.
func TestSubstringSearchForcesExpansion(t *testing.T) {
	ui, _ := testEngine(t, Options{SubstringSearch: true, PrefixSearch: "no"})
	indexDoc(t, ui, 1, nil, []string{"butterfly"})

	res, err := ui.Lookup(inbox, bodyQuery("tterf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) == 0 {
		t.Error("substring search found nothing; disabling expansion made the stored suffixes unreachable")
	}
}

// An unusable setting expands everything and says so. Narrowing on a typo would
// be reported as missing mail, which is the failure a search engine must not
// choose when in doubt.
func TestUnparseableSettingExpandsEverything(t *testing.T) {
	ui, _ := testEngine(t, Options{PrefixSearch: "maybe"})
	indexDoc(t, ui, 1, nil, []string{"butterfly"})

	res, err := ui.Lookup(inbox, bodyQuery("butterf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) == 0 {
		t.Error("an unparseable setting narrowed matching; it must fall back to expanding")
	}
}
