package buildmail

import (
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/fts/language"
)

func mustMultiChain(t *testing.T, languages ...string) *language.MultiChain {
	t.Helper()
	c, err := language.NewMultiChain(languages, []string{"lowercase", "snowball", "stopwords"}, nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const englishMsg = "Subject: weather report\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"The quick brown fox jumps over the lazy dog while the sun shines brightly over the green meadow this morning.\r\n"

const germanMsg = "Subject: wetterbericht\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Der schnelle braune Fuchs springt ueber den faulen Hund waehrend die Sonne hell ueber die gruene Wiese scheint heute Morgen.\r\n"

const ukrainianMsg = "Subject: pryvit\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Доброго дня! Сьогодні чудова погода, і я хочу піти погуляти у парку разом із друзями та випити смачної кави. У мене є цікаві книги.\r\n"

const russianMsg = "Subject: privet\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Сегодня хорошая погода, и я хочу пойти погулять в парке вместе с друзьями и выпить вкусного кофе. У меня есть интересные книги.\r\n"

// TestBuildSelectsLanguagePerMessage (#668 point 3, #696) proves detection
// actually drives indexing: the German message's "Fuchs" (fox) survives
// German stemming/stopwords, and its English translation "fox" — which
// WOULD have been stemmed under the English chain — must NOT appear, since
// this message's one body part was indexed under German, not English.
func TestBuildSelectsLanguagePerMessage(t *testing.T) {
	chain := mustMultiChain(t, "en", "de")
	b := New(Options{}, chain)

	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(germanMsg), upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "fuch") { // German snowball stem of "Fuchs"
		t.Fatalf("German message not indexed under German (missing 'fuch' stem): %q", tokens)
	}
	if hasToken(tokens, "fox") {
		t.Fatalf("German message wrongly indexed under English: %q", tokens)
	}
}

// TestBuildSingleLanguageConfigUnchanged is the control: with only "en"
// configured, detection never runs (MultiChain degenerates to the old
// single-Chain behaviour) and German text is indexed under English's
// filter chain regardless of its actual language — unchanged from before
// #668 point 3.
func TestBuildSingleLanguageConfigUnchanged(t *testing.T) {
	chain := mustMultiChain(t, "en")
	b := New(Options{}, chain)

	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(germanMsg), upd); err != nil {
		t.Fatal(err)
	}
	// No assertion on the exact stem — just confirm indexing succeeded and
	// produced tokens (proving no crash/empty-result regression when only
	// one language is configured).
	if len(upd.bodyTokens()) == 0 {
		t.Fatal("single-language config produced no body tokens")
	}
}

// TestBuildBothLanguagesIndexCorrectly is the symmetric check: an English
// message in the same multi-language config indexes under English.
func TestBuildBothLanguagesIndexCorrectly(t *testing.T) {
	chain := mustMultiChain(t, "en", "de")
	b := New(Options{}, chain)

	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(englishMsg), upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "fox") {
		t.Fatalf("English message not indexed under English (missing 'fox'): %q", tokens)
	}
}

// TestBuildMultiLanguageDoesNotBufferWholeMessage (#695, generalized to
// per-part by #696): even when detection is needed (multiple configured
// languages), the body part's own detection must only buffer a bounded
// prefix (defaultDetectionSampleBytes, plus one retry growth step) — never
// the whole part — and must still correctly select the language from that
// bounded prefix and index the rest via streaming.
func TestBuildMultiLanguageDoesNotBufferWholeMessage(t *testing.T) {
	hugePadding := strings.Repeat("x", 2_000_000) // ~2MB, far past the sample cap
	msg := germanMsg + hugePadding + "\r\n"
	upd := &fakeUpdate{}
	chain := mustMultiChain(t, "en", "de")
	// A small MaxSize makes the indexing pass itself stop early too (same
	// cap mechanism TestBuildDoesNotBufferWholeMessageForSingleLanguage
	// relies on) — isolating this test to what it actually checks: that
	// DETECTION doesn't buffer the whole part, not just that indexing
	// eventually stops reading once its own size cap is hit.
	b := New(Options{MaxSize: 100}, chain)

	br := &boundedReader{t: t, r: strings.NewReader(msg), max: int64(defaultDetectionSampleBytes*detectionRetryFactor) + 64*1024}
	if err := b.Build(1, br, upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "fuch") {
		t.Fatalf("German message not correctly detected/indexed from a bounded prefix: %q", tokens)
	}
}

// TestBuildUkrainianIndexedUnstemmed (#718) is the acceptance scenario: in a
// mixed uk/ru mailbox, a Ukrainian message is detected as uk and indexed
// WITHOUT stemming (книги stays книги, its exact lowercase form — no
// Snowball algorithm exists for Ukrainian), never mis-routed to the ru
// chain, which WOULD have stemmed the same word down to книг.
func TestBuildUkrainianIndexedUnstemmed(t *testing.T) {
	chain := mustMultiChain(t, "uk", "ru")
	b := New(Options{}, chain)

	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(ukrainianMsg), upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "книги") {
		t.Fatalf("Ukrainian message not indexed with its unstemmed form 'книги': %q", tokens)
	}
	if hasToken(tokens, "книг") {
		t.Fatalf("Ukrainian message wrongly stemmed (mis-routed to the ru chain): %q", tokens)
	}
}

// TestBuildRussianStillStemsInMixedUkRuConfig (#718) is the symmetric
// check: a Russian message in the same uk/ru config still stems normally
// (книги -> книг) — adding a stemmer-less language must not degrade an
// already-working stemmed one.
func TestBuildRussianStillStemsInMixedUkRuConfig(t *testing.T) {
	chain := mustMultiChain(t, "uk", "ru")
	b := New(Options{}, chain)

	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(russianMsg), upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "книг") {
		t.Fatalf("Russian message not stemmed (книги -> книг) in a mixed uk/ru config: %q", tokens)
	}
}

// TestBuildLanguageFiltersOverride (#726 item 4) is the integration-level
// proof that a per-language filter override actually reaches buildmail's
// indexing: with a global filter list that includes "snowball", overriding
// German to lowercase+stopwords only must index it unstemmed, while
// English (not overridden) keeps stemming via the same global list.
func TestBuildLanguageFiltersOverride(t *testing.T) {
	chain, err := language.NewMultiChain(
		[]string{"en", "de"},
		[]string{"lowercase", "snowball", "stopwords"},
		map[string][]string{"de": {"lowercase", "stopwords"}},
		0, 0, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	b := New(Options{}, chain)

	// German message: "Fuchs" must survive UNSTEMMED (override drops
	// snowball) — not the stemmed "fuch" #718's own test expects when
	// snowball IS in play.
	upd := &fakeUpdate{}
	if err := b.Build(1, strings.NewReader(germanMsg), upd); err != nil {
		t.Fatal(err)
	}
	tokens := upd.bodyTokens()
	if !hasToken(tokens, "fuchs") {
		t.Fatalf("overridden German message not indexed unstemmed (want lowercase 'fuchs'): %q", tokens)
	}
	if hasToken(tokens, "fuch") {
		t.Fatalf("overridden German message wrongly stemmed despite the override dropping snowball: %q", tokens)
	}

	// English message: not overridden, still stems via the global list.
	upd2 := &fakeUpdate{}
	if err := b.Build(2, strings.NewReader(englishMsg), upd2); err != nil {
		t.Fatal(err)
	}
	tokens2 := upd2.bodyTokens()
	if !hasToken(tokens2, "fox") {
		t.Fatalf("non-overridden English message not indexed correctly: %q", tokens2)
	}
}
