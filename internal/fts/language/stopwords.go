package language

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed data/stopwords_*.txt
var stopwordFiles embed.FS

// stopwordLists holds one word set per language, built once at package init
// from the embedded data/stopwords_<lang>.txt files (Snowball project's
// canonical stopword lists — the same project the snowball stemmers in
// filter.go come from). Every language filter.go can stem (en/fr/de/it/pt/
// ru/es) has a matching list here, so configuring any of them with the
// "stopwords" filter works — see #668 point 3.
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
