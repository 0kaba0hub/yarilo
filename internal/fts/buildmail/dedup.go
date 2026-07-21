package buildmail

import (
	"bytes"
	"hash/fnv"
)

// normalizedTextHash hashes text after collapsing all whitespace runs
// (leading/trailing included) to single-space-separated words, so a
// text/plain part and the tag-stripped text of its multipart/alternative
// text/html twin — which carry the same content but different literal
// whitespace (real newlines vs. the spaces htmlToText inserts at tag
// boundaries) — hash identically. No case-folding: that would treat
// genuinely different content (a proper noun vs. a lowercase word) as
// duplicates.
func normalizedTextHash(text []byte) uint64 {
	h := fnv.New64a()
	for i, word := range bytes.Fields(text) {
		if i > 0 {
			_, _ = h.Write([]byte{' '})
		}
		_, _ = h.Write(word)
	}
	return h.Sum64()
}
