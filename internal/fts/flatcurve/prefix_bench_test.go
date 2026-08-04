//go:build flatcurve

package flatcurve

import (
	"context"
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// benchIndex builds an index of docs messages over a vocabulary wide enough
// that a short prefix matches a large part of it — which is the condition the
// setting exists for.
func benchIndex(b *testing.B, prefix string) (fts.UserIndex, []string) {
	b.Helper()
	user := fts.UserRef{Username: "u@test", IndexRoot: b.TempDir()}
	ui, err := New(Options{PrefixSearch: prefix, CommitLimit: 5000}).OpenUser(context.Background(), user)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { ui.Close() }) //nolint:errcheck

	// 2000 distinct words sharing short prefixes: "abcd0000".."abcd1999" style,
	// so "ab" matches everything and "abcd12" matches ten.
	const docs = 400
	var vocab []string
	for i := 0; i < 2000; i++ {
		vocab = append(vocab, fmt.Sprintf("abcd%04d", i))
	}
	for d := 0; d < docs; d++ {
		up, err := ui.BeginUpdate(inbox)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := up.SetBuildKey(fts.BuildKey{UID: uint32(d + 1), Type: fts.KeyBodyPart, ContentType: "text/plain"}); err != nil {
			b.Fatal(err)
		}
		for w := 0; w < 40; w++ {
			if err := up.BuildMore([]byte(vocab[(d*40+w)%len(vocab)])); err != nil {
				b.Fatal(err)
			}
		}
		if err := up.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	return ui, vocab
}

// What a prefix search costs by term length, against exact matching.
//
// The setting decides how much of the index one search word touches, and the
// answer was hard-wired to "all of it" for every length. This is what that
// costs, so a default can be chosen from a number.
//
//	go test -tags flatcurve -run xxx -bench PrefixSearch -benchtime 50x ./internal/fts/flatcurve/
func BenchmarkPrefixSearchByTermLength(b *testing.B) {
	for _, setting := range []struct{ name, value string }{
		{"expand-every-term", "yes"},
		{"exact-only", "no"},
	} {
		ui, _ := benchIndex(b, setting.value)
		for _, term := range []string{"ab", "abcd", "abcd12", "abcd1234"} {
			b.Run(fmt.Sprintf("%s/term=%d", setting.name, len(term)), func(b *testing.B) {
				q := bodyQuery(term)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := ui.Lookup(inbox, q); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
