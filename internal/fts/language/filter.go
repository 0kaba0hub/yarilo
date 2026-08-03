package language

import (
	"fmt"
	"strings"
	"sync"

	"github.com/blevesearch/snowballstem"
	"github.com/blevesearch/snowballstem/english"
	"github.com/blevesearch/snowballstem/french"
	"github.com/blevesearch/snowballstem/german"
	"github.com/blevesearch/snowballstem/italian"
	"github.com/blevesearch/snowballstem/portuguese"
	"github.com/blevesearch/snowballstem/russian"
	"github.com/blevesearch/snowballstem/spanish"
)

// Filter transforms one token; ok=false drops the token entirely.
type Filter interface {
	Name() string
	Apply(token string) (out string, ok bool)
}

type lowercaseFilter struct{}

func (lowercaseFilter) Name() string { return "lowercase" }
func (lowercaseFilter) Apply(t string) (string, bool) {
	return strings.ToLower(t), true
}

type stopwordsFilter struct{ words map[string]struct{} }

func (stopwordsFilter) Name() string { return "stopwords" }
func (f stopwordsFilter) Apply(t string) (string, bool) {
	if _, stop := f.words[t]; stop {
		return "", false
	}
	return t, true
}

type snowballFilter struct{ stem func(*snowballstem.Env) bool }

// stemEnvs recycles Snowball execution environments. Apply runs once per token,
// so allocating one per call put a garbage-collected object in the hottest loop
// the indexer has: stemming a mail-sized token stream spent 34% of its time and
// 70% of its bytes on that single allocation.
//
// The pool rather than a field on the filter: a snowballFilter value is shared
// across the index workers and concurrent searches, and an Env carries the
// cursor state of the stem in progress. One shared Env would interleave two
// tokens into each other.
//
// SetCurrent resets every field NewEnv sets, so a recycled Env is
// indistinguishable from a fresh one.
var stemEnvs = sync.Pool{New: func() any { return snowballstem.NewEnv("") }}

func (snowballFilter) Name() string { return "snowball" }
func (f snowballFilter) Apply(t string) (string, bool) {
	env, _ := stemEnvs.Get().(*snowballstem.Env)
	env.SetCurrent(t)
	f.stem(env)
	out := env.Current()
	// Returned before the token is handed back: Current is the stemmed string,
	// and holding it in the pooled Env would keep it alive for as long as the
	// Env is idle.
	env.SetCurrent("")
	stemEnvs.Put(env)
	return out, true
}

// passthroughFilter stands in for "snowball" on a language with no
// Snowball stemmer (e.g. uk). Name() still reports "snowball" so the
// configured chain shows up unchanged in logs.
type passthroughFilter struct{}

func (passthroughFilter) Name() string                  { return "snowball" }
func (passthroughFilter) Apply(t string) (string, bool) { return t, true }

var snowballStemmers = map[string]func(*snowballstem.Env) bool{
	"en": english.Stem,
	"fr": french.Stem,
	"de": german.Stem,
	"it": italian.Stem,
	"pt": portuguese.Stem,
	"ru": russian.Stem,
	"es": spanish.Stem,
}

// buildFilters resolves the configured filter names for one language,
// mirroring the language_filters chain order.
func buildFilters(names []string, lang string) ([]Filter, error) {
	out := make([]Filter, 0, len(names))
	for _, n := range names {
		switch n {
		case "lowercase":
			out = append(out, lowercaseFilter{})
		case "snowball":
			if stem, ok := snowballStemmers[lang]; ok {
				out = append(out, snowballFilter{stem: stem})
			} else {
				out = append(out, passthroughFilter{}) // stemmer-less language
			}
		case "stopwords":
			words, ok := stopwordLists[lang]
			if !ok {
				return nil, fmt.Errorf("fts/language: no stopword list for language %q", lang)
			}
			out = append(out, stopwordsFilter{words: words})
		default:
			return nil, fmt.Errorf("fts/language: unknown filter %q", n)
		}
	}
	return out, nil
}
