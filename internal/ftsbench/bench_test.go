//go:build flatcurve

package ftsbench

import (
	"testing"
)

// indexRatioCap bounds on-disk index size as a multiple of the corpus
// (docs/FTS.md §12); catches e.g. accidental substring indexing.
const indexRatioCap = 3.0

// TestAcceptance fails when indexed SEARCH is slower than a brute-force
// scan or the index grows past indexRatioCap (docs/FTS.md §12).
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
