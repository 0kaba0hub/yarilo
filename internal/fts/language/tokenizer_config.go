package language

import "fmt"

// ValidateTokenizerConfig validates the fts_language_tokenizer_generic_*
// keys (#726 item 2). Only the "simple" algorithm (this package's own
// tokenizer.go) is implemented. "tr29" is a valid reference option, not
// silently mapped to "simple" or ignored — it errors clearly at startup
// until the TR29 tokenizer lands, currently blocked on the Bleve stream.
//
// wb5a and explicitPrefix are TR29-only knobs (WB5a word-break rule
// variant; explicit_prefix controls trailing-'*' prefix-search semantics —
// see docs/FTS.md). Both are accepted config keys but rejected if true, for
// the same reason as tr29: a silent no-op would be worse than a clear
// error, and — for explicitPrefix specifically — flatcurve already
// prefix-matches every term unconditionally (its own Xapian OP_WILDCARD
// query shape), so enabling the knob today would have no visible effect
// even if it didn't error, which is exactly the kind of "works by
// accident" behavior worth refusing outright.
func ValidateTokenizerConfig(algorithm string, wb5a, explicitPrefix bool) error {
	switch algorithm {
	case "", "simple":
	case "tr29":
		return fmt.Errorf("fts/language: fts_language_tokenizer_generic_algorithm=tr29 is not yet implemented (blocked on the Bleve stream)")
	default:
		return fmt.Errorf("fts/language: unknown fts_language_tokenizer_generic_algorithm %q", algorithm)
	}
	if wb5a {
		return fmt.Errorf("fts/language: fts_language_tokenizer_generic_wb5a is not yet implemented (TR29-only, blocked on the Bleve stream)")
	}
	if explicitPrefix {
		return fmt.Errorf("fts/language: fts_language_tokenizer_generic_explicit_prefix is not yet implemented (TR29-only, blocked on the Bleve stream)")
	}
	return nil
}
