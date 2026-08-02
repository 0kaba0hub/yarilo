//go:build flatcurve

// fts-bench generates a synthetic mail corpus under --root, indexes it through
// the embedded yarilo-fts service (flatcurve engine), and reports the two
// acceptance axes — SEARCH latency indexed-vs-scan and index size — plus
// indexing throughput. Point --root at the NFS volume in the sandbox to
// measure real-storage behaviour. See docs/FTS.md §15.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/yarilomail/yarilo/internal/ftsbench"
)

func main() {
	var (
		root     = flag.String("root", "", "mail root (required; use an NFS path in the sandbox)")
		corpus   = flag.Int("corpus", 5000, "number of messages to generate")
		hitEvery = flag.Int("hit-every", 100, "inject the search needle into 1 in N messages")
		iters    = flag.Int("iters", 50, "SEARCH repetitions for the p95 latency")
		report   = flag.String("report", "", "optional path to write the JSON report")
	)
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "fts-bench: --root is required")
		os.Exit(2)
	}

	rep, err := ftsbench.Run(ftsbench.Config{
		Root: *root, Corpus: *corpus, HitEvery: *hitEvery, Iterations: *iters,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fts-bench:", err)
		os.Exit(1)
	}
	fmt.Print(rep.String())
	if *report != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*report, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fts-bench: write report:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "report written to %s\n", *report)
	}
}
