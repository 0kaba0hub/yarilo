//go:build flatcurve

package flatcurve

import "testing"

// The minimum term size is a judgement about what is worth indexing, and that
// judgement is the same in every script. Counting bytes made it a different
// rule per script: a one-character Latin word was dropped and a one-character
// Cyrillic or CJK one was kept, because their bytes outnumbered their
// characters (#1055).
//
// Asserted per script rather than as one case, because the defect is invisible
// in any single alphabet — every ASCII example behaves identically under both
// readings.
func TestMinTermSizeCountsCharacters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		term    string
		minSize int
		kept    bool
	}{
		// One character. Under byte counting the first is dropped and the rest
		// are kept, which is the whole defect in four lines.
		{"latin, one character", "a", 2, false},
		{"cyrillic, one character", "я", 2, false},
		{"cjk, one character", "日", 2, false},
		{"greek, one character", "α", 2, false},

		// Two characters, at the threshold.
		{"latin, two characters", "by", 2, true},
		{"cyrillic, two characters", "як", 2, true},

		// A higher threshold widens the asymmetry: under byte counting a
		// four-letter Latin word was dropped while a two-letter Cyrillic one
		// was kept.
		{"latin, four characters at min 4", "word", 4, true},
		{"latin, three characters at min 4", "and", 4, false},
		{"cyrillic, two characters at min 4", "як", 4, false},
		{"cyrillic, four characters at min 4", "мова", 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normTerm(tc.term, tc.minSize)
			if kept := got != ""; kept != tc.kept {
				t.Errorf("normTerm(%q, %d) = %q, want kept=%v (%d characters, %d bytes)",
					tc.term, tc.minSize, got, tc.kept, len([]rune(tc.term)), len(tc.term))
			}
		})
	}
}

// The storage bound stays in bytes, which is correct and worth pinning: it is
// about how much room a term takes, not about whether it is worth having.
func TestMaxTermLengthStaysInBytes(t *testing.T) {
	// Multi-byte characters, well past the byte bound but not past it in
	// characters.
	long := ""
	for len(long) <= maxTermBytes {
		long += "я"
	}
	got := normTerm(long, 2)
	if len(got) > maxTermBytes {
		t.Errorf("term of %d bytes was not truncated to the %d-byte bound", len(got), maxTermBytes)
	}
	if got == "" {
		t.Error("a long term was dropped rather than truncated")
	}
	// And the truncation must not leave a partial character behind.
	for i, r := range got {
		if r == 0xFFFD {
			t.Errorf("truncation left an invalid rune at byte %d", i)
		}
	}
}

// Substring indexing stops emitting suffixes at the same threshold, so it has
// to count the same way. Under byte counting a Cyrillic word emitted suffixes
// down to a single character.
func TestSubstringSuffixesStopAtTheCharacterThreshold(t *testing.T) {
	ui, _ := testEngine(t, Options{SubstringSearch: true, MinTermSize: 3, PrefixSearch: "yes"})
	indexDoc(t, ui, 1, nil, []string{"мова"})

	for _, tc := range []struct {
		fragment string
		found    bool
	}{
		{"мова", true}, // the term
		{"ова", true},  // 3 characters — at the threshold
		{"ва", false},  // 2 characters — below it, 4 bytes
	} {
		res, err := ui.Lookup(inbox, bodyQuery(tc.fragment))
		if err != nil {
			t.Fatal(err)
		}
		if found := len(res.Definite) > 0; found != tc.found {
			t.Errorf("fragment %q found=%v, want %v (%d characters, %d bytes)",
				tc.fragment, found, tc.found, len([]rune(tc.fragment)), len(tc.fragment))
		}
	}
}
