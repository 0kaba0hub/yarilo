package buildmail

import (
	"bytes"
	"hash/fnv"
)

// normalizedTextHash hashes text with whitespace runs collapsed, so a
// text/plain part and the tag-stripped text of its text/html alternative
// hash identically. No case-folding: that would fuse distinct content.
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
