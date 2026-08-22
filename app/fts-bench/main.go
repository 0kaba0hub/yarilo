//go:build flatcurve

// fts-bench generates a synthetic mail corpus under --root, indexes it through
// the embedded yarilo-fts service (flatcurve engine), and reports the two
// acceptance axes — SEARCH latency indexed-vs-scan and index size — plus
// indexing throughput. Point --root at the NFS volume in the sandbox to
// measure real-storage behaviour. See https://doc.yarilomail.org/FTS §15.
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
		mode     = flag.String("mode", "search", "search: the acceptance axes; reopen: commit vs write-handle reopen cost (#1397)")
		batches  = flag.Int("batches", 20, "reopen mode: commit batches per phase")
		perBatch = flag.Int("docs-per-batch", 500, "reopen mode: documents between commits (models fts_commit_limit)")
		coldBox  = flag.Int("cold-boxes", 20, "reopen mode: distinct mailboxes for the cold-open phase")
	)
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "fts-bench: --root is required")
		os.Exit(2)
	}

	if *mode == "reopen" {
		rep, err := ftsbench.RunReopen(ftsbench.ReopenConfig{
			Root: *root, Batches: *batches, DocsPerBatch: *perBatch, ColdBoxes: *coldBox,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fts-bench:", err)
			os.Exit(1)
		}
		fmt.Print(rep.String())
		writeReport(*report, rep)
		return
	}

	rep, err := ftsbench.Run(ftsbench.Config{
		Root: *root, Corpus: *corpus, HitEvery: *hitEvery, Iterations: *iters,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fts-bench:", err)
		os.Exit(1)
	}
	fmt.Print(rep.String())
	writeReport(*report, rep)
}

func writeReport(path string, rep any) {
	if path == "" {
		return
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fts-bench: write report:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "report written to %s\n", path)
}
