//go:build flatcurve

package ftsbench

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/0kaba0hub/yarilo/internal/fts/flatcurve"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/internal/ftsservice"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

const benchUser = "bench@example.com"

var benchMbox = fts.MailboxRef{Name: "INBOX", UIDValidity: 1}

// Config parameterises a run. Root is the mail root — point it at an NFS
// volume in the sandbox to measure real-storage behaviour.
type Config struct {
	Root       string
	Corpus     int
	HitEvery   int
	Iterations int // SEARCH repetitions for the latency percentiles
}

// Run generates the corpus under cfg.Root, indexes it, and measures the
// index-backed SEARCH against the brute-force scan. The returned Report holds
// the two acceptance axes (latency, index size) plus throughput.
func Run(cfg Config) (Report, error) {
	if cfg.Corpus <= 0 {
		cfg.Corpus = 2000
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 50
	}
	corpus := Generate(cfg.Corpus, cfg.HitEvery)

	resolver := &mailbox.Resolver{Root: cfg.Root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo(benchUser, "")
	mb := maildir.New()
	idx := file.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		return Report{}, fmt.Errorf("ftsbench: init mailbox: %w", err)
	}
	defer box.Close() //nolint:errcheck
	uidx := idx.OpenUser(info)
	defer uidx.Close() //nolint:errcheck

	folder, err := uidx.OpenFolder(benchMbox.Name, benchMbox.UIDValidity)
	if err != nil {
		return Report{}, fmt.Errorf("ftsbench: open folder: %w", err)
	}
	metas := make([]*mailbox.MessageMeta, 0, len(corpus.Messages))
	for _, m := range corpus.Messages {
		name, err := box.Save(benchMbox.Name, bytes.NewReader(m.Raw), m.UID, int64(len(m.Raw)), nil)
		if err != nil {
			return Report{}, fmt.Errorf("ftsbench: save uid %d: %w", m.UID, err)
		}
		meta := &mailbox.MessageMeta{UID: m.UID, Filename: name, Size: uint32(len(m.Raw))}
		if err := uidx.AppendMessage(folder.ID, meta); err != nil {
			return Report{}, fmt.Errorf("ftsbench: append uid %d: %w", m.UID, err)
		}
		metas = append(metas, meta)
	}

	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		return Report{}, fmt.Errorf("ftsbench: language chain: %w", err)
	}
	svc, err := ftsservice.New(ftsservice.Options{
		Engine:      flatcurve.New(flatcurve.Options{}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
		Chain:       chain,
		CommitLimit: 500,
	})
	if err != nil {
		return Report{}, fmt.Errorf("ftsbench: service: %w", err)
	}
	defer svc.Close() //nolint:errcheck

	maxUID := uint32(len(corpus.Messages))
	start := time.Now()
	if err := svc.Index(benchUser, benchMbox, maxUID, 0); err != nil {
		return Report{}, fmt.Errorf("ftsbench: index: %w", err)
	}
	if err := waitIndexed(svc, maxUID, 5*time.Minute); err != nil {
		return Report{}, err
	}
	indexElapsed := time.Since(start)

	query := fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldBody, Words: chain.ExpandSearch(Needle)}},
		AndTerms: true,
	}
	// Correctness gate first: the index must return exactly the injected hits.
	res, err := svc.Lookup(benchUser, benchMbox, query)
	if err != nil {
		return Report{}, fmt.Errorf("ftsbench: lookup: %w", err)
	}
	if err := checkHits(res.Definite, corpus.Hits); err != nil {
		return Report{}, err
	}

	indexedP95 := measure(cfg.Iterations, func() {
		_, _ = svc.Lookup(benchUser, benchMbox, query)
	})
	criteria := &imaplib.SearchCriteria{Body: []string{Needle}}
	scanP95 := measure(cfg.Iterations, func() {
		scanOnce(box, benchMbox.Name, metas, criteria)
	})

	indexBytes, err := dirSize(cfg.Root, flatcurve.Label)
	if err != nil {
		return Report{}, err
	}

	rep := Report{
		Corpus:           len(corpus.Messages),
		Hits:             len(corpus.Hits),
		CorpusBytes:      corpus.TotalBytes,
		IndexBytes:       indexBytes,
		IndexRatio:       ratio(indexBytes, corpus.TotalBytes),
		IndexThroughput:  float64(maxUID) / indexElapsed.Seconds(),
		ScanP95Millis:    float64(scanP95.Microseconds()) / 1000,
		IndexedP95Millis: float64(indexedP95.Microseconds()) / 1000,
		Speedup:          ratioDur(scanP95, indexedP95),
	}
	return rep, nil
}

// scanOnce reproduces the brute-force SEARCH path: fetch every message and
// match it against the criteria.
func scanOnce(box mailbox.UserMailbox, folder string, metas []*mailbox.MessageMeta, criteria *imaplib.SearchCriteria) {
	for i, m := range metas {
		rc, err := box.Fetch(folder, m.Filename, m.AltTier)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(rc)
		rc.Close() //nolint:errcheck
		imapserver.MatchMessage(uint32(i+1), imaplib.UID(m.UID), time.Time{},
			int64(m.Size), nil, raw, criteria)
	}
}

func measure(iters int, fn func()) time.Duration {
	ds := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		t := time.Now()
		fn()
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	idx := (len(ds) * 95) / 100
	if idx >= len(ds) {
		idx = len(ds) - 1
	}
	return ds[idx]
}

func waitIndexed(svc *ftsservice.Service, target uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		last, _, err := svc.Status(benchUser, benchMbox)
		if err == nil && last >= target {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("ftsbench: indexing did not reach uid %d within %s", target, timeout)
}

func checkHits(got, want []uint32) error {
	if len(got) != len(want) {
		return fmt.Errorf("ftsbench: index returned %d hits, want %d", len(got), len(want))
	}
	g := append([]uint32(nil), got...)
	sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
	for i := range want {
		if g[i] != want[i] {
			return fmt.Errorf("ftsbench: hit set mismatch at %d: got %d want %d", i, g[i], want[i])
		}
	}
	return nil
}

// dirSize sums the bytes of every file living under a directory named label
// anywhere in the tree (the per-mailbox fts-flatcurve shards).
func dirSize(root, label string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+label+string(os.PathSeparator)) {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func ratioDur(a, b time.Duration) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
