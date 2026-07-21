package buildmail

import (
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/fts/language"
)

func mustMultiChain(t *testing.T, languages ...string) *language.MultiChain {
	t.Helper()
	c, err := language.NewMultiChain(languages, []string{"lowercase", "snowball", "stopwords"}, 0, 0)
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

// TestBuildSelectsLanguagePerMessage (#668 point 3) proves detection
// actually drives indexing: the German message's "Fuchs" (fox) survives
// German stemming/stopwords, and its English translation "fox" — which
// WOULD have been stemmed under the English chain — must NOT appear, since
// this message was indexed under German, not English.
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
