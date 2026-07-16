package language

import (
	"fmt"
	"strings"

	"github.com/blevesearch/snowballstem"
	"github.com/blevesearch/snowballstem/english"
	"github.com/blevesearch/snowballstem/french"
	"github.com/blevesearch/snowballstem/german"
	"github.com/blevesearch/snowballstem/italian"
	"github.com/blevesearch/snowballstem/portuguese"
	"github.com/blevesearch/snowballstem/russian"
	"github.com/blevesearch/snowballstem/spanish"
)

// Filter transforms one token; ok=false drops the token entirely
// (the lang_filter chain contract).
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

func (snowballFilter) Name() string { return "snowball" }
func (f snowballFilter) Apply(t string) (string, bool) {
	env := snowballstem.NewEnv(t)
	f.stem(env)
	return env.Current(), true
}

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
			stem, ok := snowballStemmers[lang]
			if !ok {
				return nil, fmt.Errorf("fts/language: no snowball stemmer for language %q", lang)
			}
			out = append(out, snowballFilter{stem: stem})
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
