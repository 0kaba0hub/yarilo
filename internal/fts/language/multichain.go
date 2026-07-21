package language

import "github.com/0kaba0hub/yarilo/pkg/fts"

// MultiChain holds one Chain per configured language and implements the
// reference implementation's deliberately ASYMMETRIC multi-language design
// (#668 point 3):
//
//   - Indexing selects exactly ONE language per message — auto-detected,
//     falling back to the first configured language on short/ambiguous text
//     — and applies only that language's stemmer/stopwords. No redundant
//     per-language re-stemming at index time.
//   - Search expands each query token through EVERY configured language's
//     filter chain, OR-ing the results together ("enough for one of them to
//     match" — the reference implementation's own phrasing), so a query
//     matches content indexed under any configured language without needing
//     to know which one a given message was detected as.
//
// A single configured language degenerates MultiChain to exactly the old
// single-Chain behaviour (chains[0] always selected, no detector call) —
// one code path serves both cases, per the config-not-binary principle.
type MultiChain struct {
	chains    []*Chain
	languages []string // parallel to chains; chains[0]/languages[0] is the fallback
}

// NewMultiChain builds one Chain per language, sharing the same filter set
// and token/address limits. languages must be non-empty; the first entry is
// the fallback used when detection is skipped (single language configured)
// or unreliable.
func NewMultiChain(languages []string, filters []string, tokenMaxLen, addressMaxLen int) (*MultiChain, error) {
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	m := &MultiChain{
		chains:    make([]*Chain, 0, len(languages)),
		languages: make([]string, 0, len(languages)),
	}
	for _, lang := range languages {
		c, err := NewChain(Settings{
			Language:      lang,
			Filters:       filters,
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

// SelectForIndex picks the single chain a message's index tokens go
// through. sample should be a representative excerpt of the message (e.g.
// subject + the start of the body) — enough text for reliable detection,
// but not the whole message. Returns the chosen chain and its language code
// (for logging/observability — see #625-style tracing conventions).
func (m *MultiChain) SelectForIndex(sample string) (*Chain, string) {
	if len(m.chains) == 1 {
		return m.chains[0], m.languages[0]
	}
	if lang, ok := detectLanguage(sample, m.languages); ok {
		for i, l := range m.languages {
			if l == lang {
				return m.chains[i], lang
			}
		}
	}
	return m.chains[0], m.languages[0]
}

// ExpandSearch mirrors Chain.ExpandSearch, but for each token adds every
// configured language's filtered form as an additional OR-variant on the
// same Word (Word.Variants is already documented as an OR set — a document
// matches when it matches ANY variant). A token that every configured
// language's filter chain treats as a stopword is dropped from the query
// entirely, same as the single-language case, since it was never indexed
// under any of them.
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
			continue // stopword in every configured language: never indexed anywhere
		}
		words = append(words, fts.Word{Variants: variants})
	}
	return words
}

// SettingsChecksum aggregates every configured chain's checksum, so a
// mailbox checkpoint invalidates (forcing a rebuild) when the configured
// language SET changes — adding, removing, or reordering a language, not
// just changing the single active one.
func (m *MultiChain) SettingsChecksum() uint32 {
	h := uint32(2166136261) // FNV-1a
	for _, c := range m.chains {
		cc := c.SettingsChecksum()
		for i := 0; i < 4; i++ {
			h ^= uint32(byte(cc >> (8 * i)))
			h *= 16777619
		}
	}
	return h
}
