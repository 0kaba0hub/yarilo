package language

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed data/stopwords_*.txt
var stopwordFiles embed.FS

// stopwordLists: one word set per language, from the embedded Snowball
// stopword lists. Every stemmable language in filter.go has a list here.
var stopwordLists = mustLoadStopwordLists()

func mustLoadStopwordLists() map[string]map[string]struct{} {
	entries, err := stopwordFiles.ReadDir("data")
	if err != nil {
		panic(fmt.Sprintf("fts/language: read embedded stopwords dir: %v", err))
	}
	out := make(map[string]map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name() // "stopwords_en.txt"
		lang, ok := strings.CutPrefix(name, "stopwords_")
		if !ok {
			continue
		}
		lang, ok = strings.CutSuffix(lang, ".txt")
		if !ok {
			continue
		}
		data, err := stopwordFiles.ReadFile("data/" + name)
		if err != nil {
			panic(fmt.Sprintf("fts/language: read embedded %s: %v", name, err))
		}
		out[lang] = buildStopwords(string(data))
	}
	return out
}

func buildStopwords(list string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, w := range strings.Fields(list) {
		out[w] = struct{}{}
	}
	return out
}
