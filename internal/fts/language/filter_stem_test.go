package language

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/blevesearch/snowballstem"
)

// stemCorpus is a token stream of the shape a mail body produces: inflected
// words across every language that has a stemmer, so a reused Env is exercised
// against every stem function rather than one.
var stemCorpus = []struct {
	lang   string
	tokens []string
}{
	{"en", []string{"running", "consignment", "flies", "happiness", "argued", "ponies", "caresses", "meetings"}},
	{"de", []string{"laufen", "häuser", "wissenschaftlich", "grössten", "büchern"}},
	{"fr", []string{"continuer", "chevaux", "finissions", "voudrions"}},
	{"ru", []string{"выполнение", "сообщения", "работающий", "письмами"}},
	{"es", []string{"corriendo", "naciones", "hablábamos"}},
	{"it", []string{"correre", "nazionale", "parlavamo"}},
	{"pt", []string{"correndo", "nacionalidade", "falávamos"}},
}

// The whole point of reusing the Env is that nothing observable changes. A
// stemmer whose output shifted would silently invalidate every index built
// before the change, and search would go quiet rather than break loudly.
func TestPooledEnvStemsIdenticallyToAFreshOne(t *testing.T) {
	for _, tc := range stemCorpus {
		stem, ok := snowballStemmers[tc.lang]
		if !ok {
			t.Fatalf("no stemmer for %q; the corpus and the stemmer table disagree", tc.lang)
		}
		f := snowballFilter{stem: stem}
		for _, token := range tc.tokens {
			// The reference is the previous implementation, verbatim.
			ref := snowballstem.NewEnv(token)
			stem(ref)
			want := ref.Current()

			got, ok := f.Apply(token)
			if !ok {
				t.Errorf("%s/%q: Apply dropped the token", tc.lang, token)
			}
			if got != want {
				t.Errorf("%s/%q: pooled Env stems to %q, a fresh one to %q", tc.lang, token, got, want)
			}
		}
	}
}

// A recycled Env must not carry anything from the token before it. Applying the
// same filter to a long token and then a short one is where leftover cursor
// state shows up.
func TestPooledEnvCarriesNothingBetweenTokens(t *testing.T) {
	f := snowballFilter{stem: snowballStemmers["en"]}
	sequence := []string{"internationalisation", "a", "consignment", "be", "flies"}

	for _, token := range sequence {
		ref := snowballstem.NewEnv(token)
		snowballStemmers["en"](ref)
		if got, _ := f.Apply(token); got != ref.Current() {
			t.Errorf("%q after the preceding tokens stems to %q, want %q — state leaked between calls",
				token, got, ref.Current())
		}
	}
}

// The measurement the change exists for: one allocation per token, gone.
func TestApplyDoesNotAllocatePerToken(t *testing.T) {
	f := snowballFilter{stem: snowballStemmers["en"]}
	tokens := []string{"running", "consignment", "flies", "happiness", "argued"}

	// Warm the pool, so the first Get is not counted as the steady state.
	for _, token := range tokens {
		f.Apply(token)
	}

	var i int
	allocs := testing.AllocsPerRun(1000, func() {
		f.Apply(tokens[i%len(tokens)])
		i++
	})
	// Not zero: the stemmers build their result string, which is the work
	// itself. The Env was the avoidable one, and it was exactly one per token.
	if allocs >= 4 {
		t.Errorf("Apply allocates %.1f objects per token; the pooled Env is not being reused", allocs)
	}
}

// snowballFilter is shared across index workers and concurrent searches. A
// single mutable Env would interleave two tokens into each other — a corruption
// that produces plausible words, so nothing would report it. This is the test
// that has to run under -race.
func TestApplyIsSafeUnderConcurrentUse(t *testing.T) {
	f := snowballFilter{stem: snowballStemmers["en"]}

	// Distinct per goroutine, so an interleaved Env yields a token belonging to
	// somebody else rather than an identical one.
	want := map[string]string{}
	tokens := make([]string, 32)
	for i := range tokens {
		tokens[i] = strings.Repeat("ab", i+1) + "ing"
		ref := snowballstem.NewEnv(tokens[i])
		snowballStemmers["en"](ref)
		want[tokens[i]] = ref.Current()
	}

	var wg sync.WaitGroup
	errs := make(chan string, len(tokens)*8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				token := tokens[n%len(tokens)]
				if got, _ := f.Apply(token); got != want[token] {
					errs <- fmt.Sprintf("%q stemmed to %q, want %q", token, got, want[token])
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// The lever this change was taken for. Run with -benchmem; the reference arm is
// the previous implementation, so the two arms measure the Env allocation and
// nothing else.
func BenchmarkStemTokenStream(b *testing.B) {
	stem := snowballStemmers["en"]
	f := snowballFilter{stem: stem}

	// ~620 word-like tokens, the shape of a 4 KB mail body.
	var tokens []string
	base := []string{"running", "consignment", "flies", "happiness", "argued",
		"ponies", "caresses", "meetings", "national", "generously"}
	for len(tokens) < 620 {
		tokens = append(tokens, base...)
	}

	b.Run("pooled", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, t := range tokens {
				f.Apply(t)
			}
		}
	})
	b.Run("env-per-token", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, t := range tokens {
				env := snowballstem.NewEnv(t)
				stem(env)
				_ = env.Current()
			}
		}
	})
}
