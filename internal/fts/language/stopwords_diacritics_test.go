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
