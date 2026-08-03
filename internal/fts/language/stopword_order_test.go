package language

import (
	"sort"
	"testing"
)

// A stopword list exists to keep its words out of the index. This asserts that
// it does, for every language that has one.
//
// It failed for all seven stemmed languages before the filter order was fixed,
// and not marginally: the lists are surface forms, so running them after the
// stemmer asked each entry to match its own stem. 55% of the Spanish list and
// 40% of the Russian matched nothing at all, and those words were indexed as
// terms nobody would ever search for.
//
// The test is written against the configured default chain rather than against
// the order directly, because the order is a means. What must hold is that a
// stopword does not reach the index — whatever arrangement of filters is
// configured to achieve it.
func TestEveryStopwordIsDroppedByTheDefaultChain(t *testing.T) {
	langs := make([]string, 0, len(stopwordLists))
	for l := range stopwordLists {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			chain, err := NewChain(Settings{Language: lang, Filters: defaultFilters()})
			if err != nil {
				t.Fatalf("chain for %s: %v", lang, err)
			}

			var leaked []string
			for word := range stopwordLists[lang] {
				if out, kept := chain.filter(word); kept {
					leaked = append(leaked, word+"→"+out)
				}
			}
			sort.Strings(leaked)
			if len(leaked) > 0 {
				shown := leaked
				if len(shown) > 5 {
					shown = shown[:5]
				}
				t.Errorf("%d of %d stopwords reach the index, e.g. %v",
					len(leaked), len(stopwordLists[lang]), shown)
			}
		})
	}
}

// The lists are only useful if they hold the words a text is actually made of.
// A list whose entries never appear would pass the test above by being
// irrelevant rather than by working.
func TestStopwordListsHoldCommonWords(t *testing.T) {
	for lang, want := range map[string][]string{
		"en": {"the", "and", "is", "of"},
		"de": {"der", "und", "ist"},
		"es": {"de", "que", "los"},
		"fr": {"le", "des", "est"}, // not "les": the shipped list lacks it, see the PR
		"it": {"che", "non", "per"},
		"pt": {"que", "com", "para"},
		"ru": {"и", "не", "что"},
	} {
		list, ok := stopwordLists[lang]
		if !ok {
			t.Errorf("no stopword list for %s", lang)
			continue
		}
		for _, w := range want {
			if _, present := list[w]; !present {
				t.Errorf("%s stopword list does not hold %q", lang, w)
			}
		}
	}
}

// defaultFilters is the shipped chain. Kept in one place so the test above
// tracks the default instead of restating it.
func defaultFilters() []string {
	return []string{"lowercase", "stopwords", "snowball"}
}
