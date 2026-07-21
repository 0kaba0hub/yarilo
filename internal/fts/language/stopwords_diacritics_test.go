package language

import "testing"

// TestStopwordListsIncludeDiacriticForms (review finding): the tokenizer's
// lowercase filter does not strip diacritics (strings.ToLower only), so a
// stopword list containing only the ASCII transliteration of an accented
// word (e.g. "fuer" instead of "für") never matches the real token from
// actual text and silently fails to filter it. Every accented stopword must
// have its real Unicode form present too.
func TestStopwordListsIncludeDiacriticForms(t *testing.T) {
	tests := []struct {
		lang string
		word string
	}{
		{"de", "für"},
		{"de", "über"},
		{"de", "können"},
		{"de", "während"},
		{"de", "würde"},
		{"fr", "même"},
		{"fr", "été"},
		{"fr", "êtes"},
		{"pt", "não"},
		{"pt", "está"},
		{"pt", "também"},
		{"es", "más"},
		{"es", "está"},
		{"es", "también"},
		{"it", "perché"},
		{"it", "più"},
	}
	for _, tc := range tests {
		t.Run(tc.lang+"/"+tc.word, func(t *testing.T) {
			words, ok := stopwordLists[tc.lang]
			if !ok {
				t.Fatalf("no stopword list for %q", tc.lang)
			}
			if _, stop := words[tc.word]; !stop {
				t.Errorf("%q not recognized as a stopword in %q — only its ASCII transliteration may be listed", tc.word, tc.lang)
			}
		})
	}
}

// TestStopwordListsAreCanonicalLength (#694): de/fr/es/pt were previously
// small, heavily truncated hand-picked subsets (e.g. French at ~40 words
// against the canonical Snowball list's ~150+) — not just missing accents,
// but missing most of the list entirely. Pins a minimum word count per
// language so a future accidental truncation regresses loudly instead of
// silently shrinking the filter back down.
func TestStopwordListsAreCanonicalLength(t *testing.T) {
	minWords := map[string]int{
		"en": 100,
		"de": 150,
		"fr": 100,
		"es": 200,
		"pt": 150,
		"it": 80,
		"ru": 80,
	}
	for lang, min := range minWords {
		words, ok := stopwordLists[lang]
		if !ok {
			t.Errorf("no stopword list for %q", lang)
			continue
		}
		if len(words) < min {
			t.Errorf("stopwordLists[%q] has %d words, want at least %d (canonical Snowball list length)", lang, len(words), min)
		}
	}
}
