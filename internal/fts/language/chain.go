package language

import (
	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// Settings selects the tokenizer limits and the filter chain.
type Settings struct {
	// Language is the ISO code the snowball / stopwords filters key on.
	Language string
	// Filters is the ordered filter chain (lowercase, snowball, stopwords).
	Filters []string
	// TokenMaxLen / AddressMaxLen are byte caps (0 = reference defaults).
	TokenMaxLen   int
	AddressMaxLen int
}

// DefaultSettings mirrors docs/FTS.md: stemming and stopwords on by default —
// deliberately stronger than Dovecot's empty default filter chain.
func DefaultSettings() Settings {
	return Settings{
		Language: "en",
		Filters:  []string{"lowercase", "snowball", "stopwords"},
	}
}

// Chain is one language's tokenizer + filter pipeline, shared by indexing
// and search so both sides transform text identically.
type Chain struct {
	set     Settings
	filters []Filter
}

// NewChain validates the settings and builds the filter chain.
func NewChain(set Settings) (*Chain, error) {
	if set.Language == "" {
		set.Language = "en"
	}
	filters, err := buildFilters(set.Filters, set.Language)
	if err != nil {
		return nil, err
	}
	return &Chain{set: set, filters: filters}, nil
}

// filter runs the token through the chain; ok=false means dropped (stopword).
func (c *Chain) filter(tok string) (string, bool) {
	for _, f := range c.filters {
		var ok bool
		if tok, ok = f.Apply(tok); !ok {
			return "", false
		}
	}
	if tok == "" {
		return "", false
	}
	return tok, true
}

// IndexSession streams document text into filtered tokens. Tokens that the
// chain drops (stopwords) are not emitted.
type IndexSession struct {
	chain *Chain
	tok   *Address
	emit  EmitFunc
}

// NewIndexSession returns a session emitting filtered index tokens.
func (c *Chain) NewIndexSession(emit EmitFunc) *IndexSession {
	generic := NewGeneric(c.set.TokenMaxLen)
	filtered := func(t string) error {
		out, ok := c.filter(t)
		if !ok {
			return nil
		}
		return emit(out)
	}
	return &IndexSession{
		chain: c,
		tok:   NewAddress(generic, c.set.AddressMaxLen, false),
		emit:  filtered,
	}
}

// Write feeds a chunk of decoded UTF-8 document text.
func (s *IndexSession) Write(p []byte) error { return s.tok.Feed(p, s.emit) }

// Close flushes the final token.
func (s *IndexSession) Close() error { return s.tok.Flush(s.emit) }

// ExpandSearch turns one search string into the engine query shape,
// mirroring fts_backend_dovecot_tokenize_lang / expand_tokens: the string
// runs through the search-mode tokenizer; each token becomes a Word whose
// variants are {the whole original string, the tokenized-but-unfiltered
// token, the filtered token} (deduplicated); a token the filter chain drops
// (stopword) was never indexed and is removed from the query entirely;
// multi-word strings are an AND of Words.
func (c *Chain) ExpandSearch(query string) []fts.Word {
	generic := NewGeneric(c.set.TokenMaxLen)
	addr := NewAddress(generic, c.set.AddressMaxLen, true)
	var tokens []string
	collect := func(t string) error {
		tokens = append(tokens, t)
		return nil
	}
	_ = addr.Feed([]byte(query), collect)
	_ = addr.Flush(collect)

	words := make([]fts.Word, 0, len(tokens))
	for _, tok := range tokens {
		filtered, ok := c.filter(tok)
		if !ok {
			continue // dropped stopword removes the constraint entirely
		}
		variants := make([]string, 0, 3)
		variants = appendUnique(variants, query)
		variants = appendUnique(variants, tok)
		variants = appendUnique(variants, filtered)
		words = append(words, fts.Word{Variants: variants})
	}
	return words
}

func appendUnique(dst []string, s string) []string {
	if s == "" {
		return dst
	}
	for _, v := range dst {
		if v == s {
			return dst
		}
	}
	return append(dst, s)
}

// SettingsChecksum is a stable checksum over everything that changes token
// output — the analogue of Dovecot's fts_index_header settings_checksum. A
// mismatch against a mailbox checkpoint forces that mailbox's rebuild.
func (c *Chain) SettingsChecksum() uint32 {
	h := uint32(2166136261) // FNV-1a
	mixByte := func(b byte) {
		h ^= uint32(b)
		h *= 16777619
	}
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			mixByte(s[i])
		}
		mixByte(0xff)
	}
	mixInt := func(n int) {
		for i := 0; i < 4; i++ {
			mixByte(byte(n >> (8 * i)))
		}
	}
	mix(c.set.Language)
	for _, f := range c.set.Filters {
		mix(f)
	}
	mixInt(c.tokenMaxLen())
	mixInt(c.addressMaxLen())
	return h
}

func (c *Chain) tokenMaxLen() int {
	if c.set.TokenMaxLen <= 0 {
		return DefaultTokenMaxLen
	}
	return c.set.TokenMaxLen
}

func (c *Chain) addressMaxLen() int {
	if c.set.AddressMaxLen <= 0 {
		return DefaultAddressMaxLen
	}
	return c.set.AddressMaxLen
}
