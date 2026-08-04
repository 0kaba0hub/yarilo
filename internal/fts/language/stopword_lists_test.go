package language

import (
	"sort"
	"strings"
	"testing"
)

// The lists shipped before #1022 were hand-edited copies of the Snowball files,
// and the damage was invisible to anyone who did not speak the language: words
// simply missing. What was visible was the shape of the damage — one form of a
// pair present and the other gone.
//
// So the check is on form pairs rather than on content. It needs no judgement
// about which words belong in a list; it says only that a list which knows `le`
// must also know `les`, because no source omits one and keeps the other. It
// fails on every language that was edited by hand, and passes on the files as
// their sources publish them.
func TestStopwordListsAreSymmetricAcrossFormPairs(t *testing.T) {
	// Each group is a set of forms that stand or fall together: same lemma,
	// same function, and never separated by a source list.
	groups := map[string][][]string{
		"en": {
			{"i", "me", "my", "myself"},
			{"he", "him", "his", "himself"},
			{"she", "her", "hers", "herself"},
			{"they", "them", "their", "theirs", "themselves"},
			{"this", "that", "these", "those"},
			{"am", "is", "are", "was", "were", "be", "been", "being"},
			{"have", "has", "had", "having"},
			{"do", "does", "did", "doing"},
		},
		"de": {
			{"der", "die", "das", "den", "dem", "des"},
			{"ein", "eine", "einem", "einen", "einer", "eines"},
			{"dieser", "diese", "dieses", "diesem", "diesen"},
			{"mein", "meine", "meinem", "meinen", "meiner", "meines"},
			{"war", "waren", "warst"},
		},
		"es": {
			{"un", "una", "unos", "unas"},
			{"el", "la", "los", "las"},
			{"este", "esta", "estos", "estas"},
			{"mío", "mía", "míos", "mías"},
			{"soy", "eres", "es", "somos", "sois", "son"},
		},
		"fr": {
			{"le", "la", "les"},
			{"ce", "ces", "cet", "cette"},
			{"mon", "ma", "mes"},
			{"ton", "ta", "tes"},
			{"quel", "quels", "quelle", "quelles"},
		},
		"it": {
			{"il", "lo", "la", "i", "gli", "le"},
			{"un", "uno", "una"},
			{"mio", "mia", "miei", "mie"},
			{"questo", "questi", "questa", "queste"},
			{"sono", "sei", "è", "siamo", "siete"},
		},
		"pt": {
			{"o", "a", "os", "as"},
			{"um", "uma"},
			{"este", "esta", "estes", "estas"},
			{"meu", "minha", "meus", "minhas"},
			{"sou", "somos", "são"},
		},
		"uk": {
			{"і", "й", "та"},
			{"він", "вона", "воно", "вони"},
			{"цей", "ця", "це", "ці"},
			{"мій", "моя", "моє", "мої"},
			{"був", "була", "було", "були"},
		},
		"ru": {
			{"он", "она", "оно", "они"},
			{"мой", "моя", "мою"},
			{"этот", "эта", "эти", "этой", "этом", "этого"},
			{"был", "была", "были", "будет"},
		},
	}

	langs := make([]string, 0, len(groups))
	for l := range groups {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			list, ok := stopwordLists[lang]
			if !ok {
				t.Fatalf("no stopword list for %s", lang)
			}
			for _, group := range groups[lang] {
				var have, missing []string
				for _, w := range group {
					if _, in := list[w]; in {
						have = append(have, w)
					} else {
						missing = append(missing, w)
					}
				}
				// All or nothing. A group entirely absent is a source that does
				// not carry those forms, which is its business; a group split
				// down the middle is an edit.
				if len(have) > 0 && len(missing) > 0 {
					t.Errorf("%v present but %v missing — the list was edited rather than taken from its source",
						have, missing)
				}
			}
		})
	}
}

// A list that repeats itself was assembled or merged rather than taken whole.
// Harmless at runtime, since the loader builds a set — which is exactly why it
// would never be noticed without being asserted.
func TestStopwordListsHaveNoRepeatedEntries(t *testing.T) {
	for lang, raw := range rawStopwordLists() {
		seen := map[string]bool{}
		var dupes []string
		for _, w := range raw {
			if seen[w] {
				dupes = append(dupes, w)
			}
			seen[w] = true
		}
		if len(dupes) > 0 {
			sort.Strings(dupes)
			t.Errorf("%s repeats %v", lang, dupes)
		}
	}
}

// rawStopwordLists reads the embedded files without de-duplicating, which is
// the whole point: the loader builds a set, so a repeat is invisible to
// everything except a reader of the file.
func rawStopwordLists() map[string][]string {
	entries, err := stopwordFiles.ReadDir("data")
	if err != nil {
		panic(err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		lang, ok := strings.CutPrefix(e.Name(), "stopwords_")
		if !ok {
			continue
		}
		lang, ok = strings.CutSuffix(lang, ".txt")
		if !ok {
			continue
		}
		data, err := stopwordFiles.ReadFile("data/" + e.Name())
		if err != nil {
			panic(err)
		}
		out[lang] = strings.Fields(string(data))
	}
	return out
}
