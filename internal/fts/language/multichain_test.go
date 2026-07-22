package language

import "testing"

const (
	englishSample = "The quick brown fox jumps over the lazy dog while the sun shines brightly over the green meadow this morning."
	germanSample  = "Der schnelle braune Fuchs springt über den faulen Hund während die Sonne hell über die grüne Wiese scheint heute Morgen."
)

func TestMultiChainSelectForIndex_SingleLanguageSkipsDetector(t *testing.T) {
	m, err := NewMultiChain([]string{"en"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// German text, but only "en" is configured — must still select "en"
	// without even attempting detection (degenerates to the old single-Chain
	// behaviour, per the config-not-binary "one code path" design).
	_, lang := m.SelectForIndex(germanSample)
	if lang != "en" {
		t.Errorf("SelectForIndex() with a single configured language = %q, want %q", lang, "en")
	}
}

func TestMultiChainSelectForIndex_MultiLanguageDetects(t *testing.T) {
	m, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, lang := m.SelectForIndex(englishSample); lang != "en" {
		t.Errorf("SelectForIndex(english) = %q, want en", lang)
	}
	if _, lang := m.SelectForIndex(germanSample); lang != "de" {
		t.Errorf("SelectForIndex(german) = %q, want de", lang)
	}
}

func TestMultiChainSelectForIndex_FallsBackOnShortText(t *testing.T) {
	m, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Too short to classify reliably — must fall back to the FIRST
	// configured language (en), not guess.
	_, lang := m.SelectForIndex("hi")
	if lang != "en" {
		t.Errorf("SelectForIndex(short text) = %q, want fallback en", lang)
	}
}

func TestMultiChainExpandSearch_SingleLanguageMatchesChain(t *testing.T) {
	m, err := NewMultiChain([]string{"en"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewChain(Settings{Language: "en", Filters: []string{"lowercase", "snowball", "stopwords"}})
	if err != nil {
		t.Fatal(err)
	}
	got := m.ExpandSearch("running")
	want := c.ExpandSearch("running")
	if len(got) != len(want) {
		t.Fatalf("single-language MultiChain.ExpandSearch diverged from Chain.ExpandSearch: %+v vs %+v", got, want)
	}
}

func TestMultiChainExpandSearch_ORsVariantsAcrossLanguages(t *testing.T) {
	m, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// "running" is a real (non-stopword) word in English; stem it under both
	// configured languages' filter chains directly to know what to expect.
	enChain, _ := NewChain(Settings{Language: "en", Filters: []string{"lowercase", "snowball", "stopwords"}})
	deChain, _ := NewChain(Settings{Language: "de", Filters: []string{"lowercase", "snowball", "stopwords"}})
	enStem, enOK := enChain.filter("running")
	deStem, deOK := deChain.filter("running")
	if !enOK {
		t.Fatal("expected 'running' to survive the English filter chain")
	}

	words := m.ExpandSearch("running")
	if len(words) != 1 {
		t.Fatalf("ExpandSearch(\"running\") = %d words, want 1", len(words))
	}
	if !containsVariant(words[0].Variants, enStem) {
		t.Errorf("variants %v missing the English stem %q", words[0].Variants, enStem)
	}
	if deOK && !containsVariant(words[0].Variants, deStem) {
		t.Errorf("variants %v missing the German stem %q", words[0].Variants, deStem)
	}
}

func TestMultiChainExpandSearch_DropsStopwordInEveryLanguage(t *testing.T) {
	m, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// "so" is a stopword in BOTH the English and German lists — dropped in
	// every configured language, so it must never survive as a query
	// constraint (it was never indexed under any of them).
	words := m.ExpandSearch("so")
	if len(words) != 0 {
		t.Fatalf(`ExpandSearch("so") = %d words, want 0 (stopword in every configured language)`, len(words))
	}
}

func TestMultiChainExpandSearch_KeptWhenStopwordInOnlySomeLanguages(t *testing.T) {
	m, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// "the" is an English stopword but not a German one (German's list has
	// no ASCII "the") — it must survive, since a message auto-detected as
	// German would still have indexed it.
	words := m.ExpandSearch("the")
	if len(words) != 1 {
		t.Fatalf(`ExpandSearch("the") = %d words, want 1 (kept — not a stopword in every configured language)`, len(words))
	}
}

func containsVariant(variants []string, want string) bool {
	for _, v := range variants {
		if v == want {
			return true
		}
	}
	return false
}

func TestMultiChainSettingsChecksum(t *testing.T) {
	a, err := NewMultiChain([]string{"en"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewMultiChain([]string{"en"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.SettingsChecksum() == b.SettingsChecksum() {
		t.Error("adding a language must change the checksum (checkpoint invalidation)")
	}
	if a.SettingsChecksum() != c.SettingsChecksum() {
		t.Error("identical language sets must produce identical checksums")
	}
}

func TestMultiChainNeedsDetection(t *testing.T) {
	single, err := NewMultiChain([]string{"en"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if single.NeedsDetection() {
		t.Error("a single configured language must not need detection — callers should skip sampling entirely")
	}
	multi, err := NewMultiChain([]string{"en", "de"}, []string{"lowercase", "snowball", "stopwords"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !multi.NeedsDetection() {
		t.Error("multiple configured languages must need detection")
	}
}
