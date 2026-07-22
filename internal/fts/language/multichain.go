package language

import "github.com/0kaba0hub/yarilo/pkg/fts"

// detectionAlgoVersion is mixed into SettingsChecksum whenever the detection
// algorithm itself changes token output for a fixed language configuration
// (not just the configured language set) — #696 moved from one detection
// per message to one per body/attachment part, which changes which chain
// individual parts of a mixed-language message end up indexed under. Bump
// this whenever such a change happens so existing mailboxes reindex via the
// settings-drift path instead of silently keeping stale tokens.
const detectionAlgoVersion = 2

// MultiChain holds one Chain per configured language and implements the
// reference implementation's deliberately ASYMMETRIC multi-language design
// (#668 point 3, refined by #696):
//
//   - Indexing selects exactly ONE language per body/attachment part —
//     auto-detected from that part's own text, falling back to the first
//     configured language when the part's sample is short/ambiguous (see
//     buildmail, which owns the per-part sampling). Headers are not
//     language text at all and never go through detection or a MultiChain
//     language — see buildmail's dedicated data chain.
//   - Search expands each query token through EVERY configured language's
//     filter chain, OR-ing the results together ("enough for one of them to
//     match" — the reference implementation's own phrasing), so a query
//     matches content indexed under any configured language without needing
//     to know which one a given part was detected as.
//
// A single configured language degenerates MultiChain to exactly the old
// single-Chain behaviour (chains[0] always selected, no detector call) —
// one code path serves both cases, per the config-not-binary principle.
type MultiChain struct {
	chains        []*Chain
	languages     []string // parallel to chains; chains[0]/languages[0] is the fallback
	minDetectRune int
}

// NewMultiChain builds one Chain per language, sharing the same filter set
// and token/address limits. languages must be non-empty; the first entry is
// the fallback used when detection is skipped (single language configured)
// or unreliable. minDetectRunes overrides the default reliability threshold
// for the sample text handed to TryDetect/SelectForIndex (0 = package
// default, see defaultMinDetectSample); #696 makes this tunable
// (fts_detection_min_runes).
func NewMultiChain(languages []string, filters []string, tokenMaxLen, addressMaxLen, minDetectRunes int) (*MultiChain, error) {
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	m := &MultiChain{
		chains:        make([]*Chain, 0, len(languages)),
		languages:     make([]string, 0, len(languages)),
		minDetectRune: minDetectRunes,
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

// NeedsDetection reports whether TryDetect/SelectForIndex would actually use
// their sample argument — false when only one language is configured, so
// callers can skip collecting a sample entirely (buildmail's per-part
// bounded-prefix read is pure overhead when the result is always discarded).
func (m *MultiChain) NeedsDetection() bool {
	return len(m.chains) > 1
}

// TryDetect attempts language detection only, without falling back to the
// default language on failure — callers that can cheaply grow their sample
// (buildmail's retry-with-larger-prefix, #696) use this to decide whether a
// retry is worthwhile before giving up and calling SelectForIndex.
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

// SelectForIndex picks the single chain a body/attachment part's index
// tokens go through. sample should be a representative excerpt of that
// part's own text — enough for reliable detection, but not the whole part
// (see buildmail's bounded-prefix sampling, #696). Falls back to the first
// configured language when detection is unreliable. Returns the chosen
// chain and its language code (for logging/observability).
func (m *MultiChain) SelectForIndex(sample string) (*Chain, string) {
	if c, lang, ok := m.TryDetect(sample); ok {
		return c, lang
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

// SettingsChecksum aggregates every configured chain's checksum plus the
// detection algorithm version, so a mailbox checkpoint invalidates (forcing
// a rebuild) when the configured language SET changes OR when the detection
// algorithm itself changes token output for an unchanged configuration
// (#696).
func (m *MultiChain) SettingsChecksum() uint32 {
	h := uint32(2166136261) // FNV-1a
	mix := func(n uint32) {
		for i := 0; i < 4; i++ {
			h ^= uint32(byte(n >> (8 * i)))
			h *= 16777619
		}
	}
	mix(detectionAlgoVersion)
	for _, c := range m.chains {
		mix(c.SettingsChecksum())
	}
	return h
}
