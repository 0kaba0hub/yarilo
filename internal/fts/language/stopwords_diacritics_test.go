package language

import "testing"

// Lowercasing doesn't strip diacritics, so an ASCII-only transliteration
// ("fuer") never matches the real token; every accented stopword must be
// listed in its Unicode form.
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

// Pin a minimum word count per language so an accidental truncation of a
// Snowball list fails loudly.
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
