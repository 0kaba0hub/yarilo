// Package ftsbench is the FTS acceptance benchmark: it builds a synthetic
// mail corpus, indexes it through the yarilo-fts service (embedded, flatcurve
// engine), and compares an index-backed SEARCH against the brute-force scan
// on the two axes the design commits to (docs/FTS.md §12): search latency and
// index size. Shared by the CI acceptance test and the app/fts-bench binary.
//
// The corpus generator is pure Go (no build tag) so tooling can size a run
// without linking libxapian; the measurement runner is behind the flatcurve
// tag (runner.go).
package ftsbench

import (
	"fmt"
	"math/rand"
	"strings"
)

// vocab is a fixed word pool; message bodies are drawn from it so the
// tokenizer sees realistic term frequencies. The rare needle below never
// appears here.
var vocab = []string{
	"invoice", "meeting", "project", "update", "schedule", "report", "review",
	"budget", "customer", "release", "deadline", "proposal", "contract", "vendor",
	"shipment", "inventory", "payroll", "quarter", "revenue", "forecast", "roadmap",
	"backlog", "sprint", "incident", "outage", "latency", "throughput", "storage",
	"mailbox", "message", "folder", "delivery", "attachment", "signature", "policy",
}

// Needle is the rare term the acceptance query searches for. Injected into a
// controlled fraction of messages so the index is highly selective — the case
// where an index must beat a scan.
const Needle = "xylophonic"

// Message is one generated mail: the raw RFC 5322 bytes plus whether it
// carries the needle (the expected SEARCH BODY result set).
type Message struct {
	UID    uint32
	Raw    []byte
	HasHit bool
}

// Corpus is a generated set plus the derived totals a report needs.
type Corpus struct {
	Messages   []Message
	Hits       []uint32 // UIDs containing Needle, ascending
	TotalBytes int64    // sum of Raw sizes — the "corpus size" axis
}

// Generate builds n messages with a deterministic PRNG (fixed seed, so a run
// is reproducible and comparable across commits). Roughly one in hitEvery
// messages carries the needle; hitEvery<=0 defaults to 100.
func Generate(n int, hitEvery int) Corpus {
	if hitEvery <= 0 {
		hitEvery = 100
	}
	rng := rand.New(rand.NewSource(0x5EED))
	c := Corpus{Messages: make([]Message, 0, n)}
	for i := 0; i < n; i++ {
		uid := uint32(i + 1)
		hasHit := i%hitEvery == 0
		raw := buildMessage(rng, uid, hasHit)
		c.Messages = append(c.Messages, Message{UID: uid, Raw: raw, HasHit: hasHit})
		c.TotalBytes += int64(len(raw))
		if hasHit {
			c.Hits = append(c.Hits, uid)
		}
	}
	return c
}

func buildMessage(rng *rand.Rand, uid uint32, hasHit bool) []byte {
	// 40–80 body words drawn from the vocab, with the needle spliced in at a
	// random position for hit messages.
	nWords := 40 + rng.Intn(40)
	words := make([]string, 0, nWords+1)
	needleAt := -1
	if hasHit {
		needleAt = rng.Intn(nWords)
	}
	for j := 0; j < nWords; j++ {
		if j == needleAt {
			words = append(words, Needle)
		}
		words = append(words, vocab[rng.Intn(len(vocab))])
	}
	subjectWord := vocab[rng.Intn(len(vocab))]

	var b strings.Builder
	fmt.Fprintf(&b, "From: sender%d@example.com\r\n", uid)
	fmt.Fprintf(&b, "To: user@example.com\r\n")
	fmt.Fprintf(&b, "Subject: %s %d\r\n", subjectWord, uid)
	fmt.Fprintf(&b, "Message-ID: <%d@example.com>\r\n", uid)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.Join(words, " "))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// Report is the outcome of one measured run — printed by the binary and
// asserted by the acceptance test.
type Report struct {
	Corpus           int     `json:"corpus"`
	Hits             int     `json:"hits"`
	CorpusBytes      int64   `json:"corpus_bytes"`
	IndexBytes       int64   `json:"index_bytes"`
	IndexRatio       float64 `json:"index_ratio"` // index_bytes / corpus_bytes
	IndexThroughput  float64 `json:"index_throughput_msgs_per_sec"`
	ScanP95Millis    float64 `json:"scan_p95_ms"`
	IndexedP95Millis float64 `json:"indexed_p95_ms"`
	Speedup          float64 `json:"speedup"` // scan_p95 / indexed_p95
}

// String renders a compact human table.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus:      %d messages (%d hits, %.1f MiB)\n",
		r.Corpus, r.Hits, float64(r.CorpusBytes)/(1<<20))
	fmt.Fprintf(&b, "index size:  %.1f MiB (%.2fx corpus)\n",
		float64(r.IndexBytes)/(1<<20), r.IndexRatio)
	fmt.Fprintf(&b, "index rate:  %.0f msg/s\n", r.IndexThroughput)
	fmt.Fprintf(&b, "SEARCH p95:  scan %.2f ms  vs  indexed %.2f ms  (%.0fx faster)\n",
		r.ScanP95Millis, r.IndexedP95Millis, r.Speedup)
	return b.String()
}
