package language

import (
	"fmt"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// detectionAlgoVersion is mixed into SettingsChecksum whenever the build
// algorithm changes token output for a fixed language configuration (not just
// the configured language set). Bump it on any such change so existing
// mailboxes reindex via the settings-drift path instead of keeping stale
// tokens. History: v2 detects per body/attachment part rather than per
// message; v3 indexes header NAMEs separately and parses address headers as
// RFC 5322 address-lists before tokenizing.
const detectionAlgoVersion = 3

// MultiChain holds one Chain per configured language and implements a
// deliberately ASYMMETRIC multi-language design:
//
//   - Indexing selects exactly ONE language per body/attachment part,
//     auto-detected from that part's own text, falling back to the first
//     configured language when the sample is short/ambiguous (buildmail owns
//     the per-part sampling). Headers are not language text and never go
//     through detection — see buildmail's dedicated data chain.
//   - Search expands each query token through EVERY configured language's
//     filter chain, OR-ing the results, so a query matches content indexed
//     under any configured language without knowing which one a part detected
//     as.
//
// A single configured language degenerates to the plain single-Chain behaviour
// (chains[0] always selected, no detector call) — one code path serves both.
type MultiChain struct {
	chains        []*Chain
	languages     []string // parallel to chains; chains[0]/languages[0] is the fallback
	minDetectRune int
	// overridden records which languages had an explicit
	// fts_language_filters_override entry, regardless of whether its resolved
	// filter list differs from the global default. Mixed into SettingsChecksum
	// so the override's mere presence in config forces a reindex, not just its
	// resolved effect on tokens.
	overridden map[string]bool
}

// NewMultiChain builds one Chain per language, sharing the same token/address
// limits. languages must be non-empty; the first entry is the fallback used
// when detection is skipped (single language) or unreliable. minDetectRunes
// overrides the default reliability threshold for the sample handed to
// TryDetect/SelectForIndex (0 = package default, fts_detection_min_runes).
//
// filters is the default filter chain for every language; filtersOverride
// replaces it for specific languages — e.g. uk (no Snowball stemmer) shouldn't
// carry "snowball" when other languages do. An absent language uses filters
// unchanged; a present language's list is a full replacement, not a merge.
// Every filtersOverride key must name a configured language — an unknown key
// (a typo like "ukr") is a configuration error, not silently ignored.
func NewMultiChain(languages []string, filters []string, filtersOverride map[string][]string, tokenMaxLen, addressMaxLen, minDetectRunes int) (*MultiChain, error) {
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	langSet := make(map[string]bool, len(languages))
	for _, lang := range languages {
		langSet[lang] = true
	}
	for lang := range filtersOverride {
		if !langSet[lang] {
			return nil, fmt.Errorf("fts/language: fts_language_filters_override key %q is not one of the configured languages %v", lang, languages)
		}
	}
	overridden := make(map[string]bool, len(filtersOverride))
	for lang := range filtersOverride {
		overridden[lang] = true
	}
	m := &MultiChain{
		chains:        make([]*Chain, 0, len(languages)),
		languages:     make([]string, 0, len(languages)),
		minDetectRune: minDetectRunes,
		overridden:    overridden,
	}
	for _, lang := range languages {
		langFilters := filters
		if ov, ok := filtersOverride[lang]; ok {
			langFilters = ov
		}
		c, err := NewChain(Settings{
			Language:      lang,
			Filters:       langFilters,
			TokenMaxLen:   tokenMaxLen,
			AddressMaxLen: addressMaxLen,
		})
		if err != nil {
			return nil, err
		}
		m.chains = append(m.chains, c)
		m.languages = append(m.languages, lang)
	}
	return m, nil
}

// NeedsDetection reports whether TryDetect/SelectForIndex would use their
// sample argument — false with one language configured, so callers can skip
// collecting a sample entirely.
func (m *MultiChain) NeedsDetection() bool {
	return len(m.chains) > 1
}

// TryDetect attempts detection only, without falling back to the default
// language on failure — callers that can cheaply grow their sample use this to
// decide whether a retry is worthwhile before calling SelectForIndex.
// ok=false means the sample was too short/ambiguous to trust.
func (m *MultiChain) TryDetect(sample string) (chain *Chain, lang string, ok bool) {
	if len(m.chains) == 1 {
		return m.chains[0], m.languages[0], true
	}
	if lang, ok := detectLanguage(sample, m.languages, m.minDetectRune); ok {
		for i, l := range m.languages {
			if l == lang {
				return m.chains[i], lang, true
			}
		}
	}
	return nil, "", false
}

// SelectForIndex picks the single chain a body/attachment part's index tokens
// go through. sample should be a representative excerpt of the part's own text,
// enough for reliable detection but not the whole part. Falls back to the
// first configured language when detection is unreliable. Returns the chosen
// chain and its language code.
func (m *MultiChain) SelectForIndex(sample string) (*Chain, string) {
	if c, lang, ok := m.TryDetect(sample); ok {
		return c, lang
	}
	return m.chains[0], m.languages[0]
}

// ExpandSearch mirrors Chain.ExpandSearch, but for each token adds every
// configured language's filtered form as an OR-variant on the same Word
// (Word.Variants is an OR set: a document matches ANY variant). A token every
// language treats as a stopword is dropped entirely, since it was never
// indexed under any of them.
func (m *MultiChain) ExpandSearch(query string) []fts.Word {
	if len(m.chains) == 1 {
		return m.chains[0].ExpandSearch(query)
	}

	base := m.chains[0]
	generic := NewGeneric(base.tokenMaxLen())
	addr := NewAddress(generic, base.addressMaxLen(), true)
	var tokens []string
	collect := func(t string) error {
		tokens = append(tokens, t)
		return nil
	}
	_ = addr.Feed([]byte(query), collect)
	_ = addr.Flush(collect)

	words := make([]fts.Word, 0, len(tokens))
	for _, tok := range tokens {
		variants := make([]string, 0, 2+len(m.chains))
		variants = appendUnique(variants, query)
		variants = appendUnique(variants, tok)
		anyKept := false
		for _, c := range m.chains {
			if filtered, ok := c.filter(tok); ok {
				variants = appendUnique(variants, filtered)
				anyKept = true
			}
		}
		if !anyKept {
			continue // stopword in every language: never indexed anywhere
		}
		words = append(words, fts.Word{Variants: variants})
	}
	return words
}

// SettingsChecksum aggregates every chain's checksum plus the detection
// algorithm version, so a mailbox checkpoint invalidates (forcing a rebuild)
// when the configured language SET changes or when the detection algorithm
// changes token output for an unchanged configuration.
func (m *MultiChain) SettingsChecksum() uint32 {
	h := uint32(2166136261) // FNV-1a
	mix := func(n uint32) {
		for i := 0; i < 4; i++ {
			h ^= uint32(byte(n >> (8 * i)))
			h *= 16777619
		}
	}
	mix(detectionAlgoVersion)
	for i, c := range m.chains {
		mix(c.SettingsChecksum())
		if m.overridden[m.languages[i]] {
			// The override's mere presence in config is part of the
			// configuration, even when its resolved filter list matches the
			// global default — mixed in independent of the per-chain checksum
			// above (which covers only the resolved Filters/Language/limits,
			// not where they came from).
			mix(0x726)
		}
	}
	return h
}
