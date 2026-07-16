//go:build flatcurve

package ftsbench

import (
	"testing"
)

// indexRatioCap bounds the on-disk index size as a multiple of the indexed
// corpus (docs/FTS.md §12, "DB proliferation" / index-size axis). Xapian
// glass over plain text sits well under this; the cap catches a regression
// such as accidentally enabling suffix (substring) indexing.
const indexRatioCap = 3.0

// TestAcceptance is the FTS-1 acceptance gate. It fails the build when the
// index-backed SEARCH is not at least as fast as the brute-force scan, or
// when the index grows past indexRatioCap. Both are the promises §12 makes.
func TestAcceptance(t *testing.T) {
	rep, err := Run(Config{
		Root:       t.TempDir(),
		Corpus:     1500,
		HitEvery:   100,
		Iterations: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", rep.String())

	if rep.IndexedP95Millis > rep.ScanP95Millis {
		t.Errorf("indexed SEARCH p95 %.2f ms is slower than scan p95 %.2f ms — index buys nothing",
			rep.IndexedP95Millis, rep.ScanP95Millis)
	}
	if rep.IndexRatio > indexRatioCap {
		t.Errorf("index is %.2fx the corpus, over the %.1fx cap", rep.IndexRatio, indexRatioCap)
	}
	if rep.Hits == 0 {
		t.Fatal("corpus produced no hits — benchmark is not exercising SEARCH")
	}
}

func BenchmarkIndexAndSearch(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Run(Config{Root: b.TempDir(), Corpus: 1000, Iterations: 20}); err != nil {
			b.Fatal(err)
		}
	}
}
