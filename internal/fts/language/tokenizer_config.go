package language

import "fmt"

// ValidateTokenizerConfig validates the fts_language_tokenizer_generic_*
// keys. Only "simple" is implemented; "tr29" and the TR29-only knobs
// (wb5a, explicit_prefix) error at startup rather than silently no-op.
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
