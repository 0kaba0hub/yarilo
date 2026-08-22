//go:build flatcurve

package ftsbench

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// The commit path decision (#1397) turns on one ratio: what a close-and-reopen
// of the write shard costs relative to the commit it would follow. An absolute
// millisecond figure decides nothing -- the same 5ms is a tenth of a 50ms
// commit and a doubling of a 5ms one -- so this measures both against the same
// corpus, on whatever storage Root points at.
type ReopenConfig struct {
	Root         string
	Batches      int // commit batches per phase
	DocsPerBatch int // documents between commits (the fts_commit_limit being modelled)
	TokensPerDoc int
	ColdBoxes    int // distinct mailboxes for the cold-open phase
}

// ReopenReport carries both phases. Hold is today's behaviour (one write
// handle for the whole run); Release closes the handle after every commit,
// which is what #1397 would do.
type ReopenReport struct {
	Batches      int `json:"batches"`
	DocsPerBatch int `json:"docs_per_batch"`

	CommitP50Millis float64 `json:"commit_p50_millis"`
	CommitP95Millis float64 `json:"commit_p95_millis"`

	ReopenWarmP50Millis float64 `json:"reopen_warm_p50_millis"`
	ReopenWarmP95Millis float64 `json:"reopen_warm_p95_millis"`
	ReopenColdP50Millis float64 `json:"reopen_cold_p50_millis"`
	ReopenColdP95Millis float64 `json:"reopen_cold_p95_millis"`

	// OverheadPercent is the headline: reopen p50 as a percentage of commit
	// p50, i.e. what fraction the release adds to each commit.
	OverheadPercent float64 `json:"overhead_percent"`
}

func (r ReopenReport) String() string {
	return fmt.Sprintf(`fts reopen cost (%d batches x %d docs)
  commit           p50 %.2f ms   p95 %.2f ms
  reopen (warm)    p50 %.2f ms   p95 %.2f ms
  reopen (cold)    p50 %.2f ms   p95 %.2f ms
  overhead per commit: %.1f%% of the commit it follows
`, r.Batches, r.DocsPerBatch,
		r.CommitP50Millis, r.CommitP95Millis,
		r.ReopenWarmP50Millis, r.ReopenWarmP95Millis,
		r.ReopenColdP50Millis, r.ReopenColdP95Millis,
		r.OverheadPercent)
}

const reopenUser = "reopen-bench@example.com"

// RunReopen measures the two costs the commit-scoped release trades between.
//
// The method, because the obvious one is wrong: reopen is timed as the whole
// sequence a released handle pays -- Close, then OpenUser and the first
// document write -- minus that same single-document write on a handle that was
// never closed. Timing OpenUser alone reads as near-free and is not a
// measurement: the open is lazy, so the Xapian database is opened and its
// document count read on the write that follows, outside the timed region.
// Anyone repeating this without the subtraction will understate the cost
// without meaning to.
//
// Read the result as a percentage of the commit it follows, never as
// milliseconds: the reopen cost is flat in the batch size and the commit cost
// is not, so the same 1.5ms is 1.4% of a 500-document commit and 178% of a
// single-document one. Which of those the deployment actually pays is a field
// question -- fts_index_messages_total divided by fts_index_duration_seconds_count
// is the messages-per-commit that belongs in the denominator (#1397).
func RunReopen(cfg ReopenConfig) (ReopenReport, error) {
	if cfg.Batches <= 0 {
		cfg.Batches = 20
	}
	if cfg.DocsPerBatch <= 0 {
		cfg.DocsPerBatch = 500
	}
	if cfg.TokensPerDoc <= 0 {
		cfg.TokensPerDoc = 200
	}
	if cfg.ColdBoxes <= 0 {
		cfg.ColdBoxes = 20
	}

	eng := flatcurve.New(flatcurve.Options{CommitLimit: cfg.DocsPerBatch})
	user := fts.UserRef{
		Username:  reopenUser,
		IndexRoot: filepath.Join(cfg.Root, "reopen-bench"),
		Driver:    "maildir",
	}
	mbox := fts.MailboxRef{Name: "INBOX", GUID: "g-reopen", UIDValidity: 1}

	ui, err := eng.OpenUser(context.Background(), user)
	if err != nil {
		return ReopenReport{}, fmt.Errorf("ftsbench: open user: %w", err)
	}

	var commits, reopens, singles []time.Duration
	uid := uint32(1)

	for b := 0; b < cfg.Batches; b++ {
		up, err := ui.BeginUpdate(mbox)
		if err != nil {
			return ReopenReport{}, fmt.Errorf("ftsbench: begin update: %w", err)
		}
		for d := 0; d < cfg.DocsPerBatch; d++ {
			if err := writeDoc(up, uid, cfg.TokensPerDoc); err != nil {
				return ReopenReport{}, err
			}
			uid++
		}
		t0 := time.Now()
		if err := up.Commit(); err != nil {
			return ReopenReport{}, fmt.Errorf("ftsbench: commit: %w", err)
		}
		commits = append(commits, time.Since(t0))

		// A single-document write on the handle we are about to close: the
		// subtrahend, measured under the same conditions as the sequence it
		// is subtracted from.
		single, err := timeSingleWrite(ui, mbox, &uid, cfg.TokensPerDoc)
		if err != nil {
			return ReopenReport{}, err
		}
		singles = append(singles, single)

		t1 := time.Now()
		if err := ui.Close(); err != nil {
			return ReopenReport{}, fmt.Errorf("ftsbench: close user index: %w", err)
		}
		ui, err = eng.OpenUser(context.Background(), user)
		if err != nil {
			return ReopenReport{}, fmt.Errorf("ftsbench: reopen user: %w", err)
		}
		if _, err := timeSingleWrite(ui, mbox, &uid, cfg.TokensPerDoc); err != nil {
			return ReopenReport{}, err
		}
		reopens = append(reopens, time.Since(t1))
	}
	if err := ui.Close(); err != nil {
		return ReopenReport{}, fmt.Errorf("ftsbench: final close: %w", err)
	}

	cold, err := coldOpens(eng, user, cfg)
	if err != nil {
		return ReopenReport{}, err
	}

	warm := subtract(reopens, singles)
	commitP50 := pct(commits, 50)
	rep := ReopenReport{
		Batches:             cfg.Batches,
		DocsPerBatch:        cfg.DocsPerBatch,
		CommitP50Millis:     millis(commitP50),
		CommitP95Millis:     millis(pct(commits, 95)),
		ReopenWarmP50Millis: millis(pct(warm, 50)),
		ReopenWarmP95Millis: millis(pct(warm, 95)),
		ReopenColdP50Millis: millis(pct(cold, 50)),
		ReopenColdP95Millis: millis(pct(cold, 95)),
	}
	if commitP50 > 0 {
		rep.OverheadPercent = 100 * float64(pct(warm, 50)) / float64(commitP50)
	}
	return rep, nil
}

// coldOpens times the first open of a shard this process has not touched
// since it was written, one mailbox at a time.
//
// This is as cold as a bench can honestly get without root: the kernel page
// cache and the NFS attribute cache are not ours to drop from inside a pod, so
// read these as a lower bound on a genuinely cold open, not as one.
func coldOpens(eng *flatcurve.Engine, user fts.UserRef, cfg ReopenConfig) ([]time.Duration, error) {
	boxes := make([]fts.MailboxRef, cfg.ColdBoxes)
	for i := range boxes {
		boxes[i] = fts.MailboxRef{Name: "COLD" + strconv.Itoa(i), GUID: "g-cold-" + strconv.Itoa(i), UIDValidity: 1}
	}
	// Fill every mailbox first, closing each handle, so the opens below are
	// opens of an existing shard rather than creations.
	for _, mbox := range boxes {
		ui, err := eng.OpenUser(context.Background(), user)
		if err != nil {
			return nil, fmt.Errorf("ftsbench: cold fill open: %w", err)
		}
		up, err := ui.BeginUpdate(mbox)
		if err != nil {
			return nil, fmt.Errorf("ftsbench: cold fill update: %w", err)
		}
		uid := uint32(1)
		for d := 0; d < cfg.DocsPerBatch; d++ {
			if err := writeDoc(up, uid, cfg.TokensPerDoc); err != nil {
				return nil, err
			}
			uid++
		}
		if err := up.Commit(); err != nil {
			return nil, fmt.Errorf("ftsbench: cold fill commit: %w", err)
		}
		if err := ui.Close(); err != nil {
			return nil, fmt.Errorf("ftsbench: cold fill close: %w", err)
		}
	}

	var ds []time.Duration
	for _, mbox := range boxes {
		uid := uint32(9000)
		ui, err := eng.OpenUser(context.Background(), user)
		if err != nil {
			return nil, fmt.Errorf("ftsbench: cold open: %w", err)
		}
		t0 := time.Now()
		if _, err := timeSingleWrite(ui, mbox, &uid, cfg.TokensPerDoc); err != nil {
			return nil, err
		}
		ds = append(ds, time.Since(t0))
		if err := ui.Close(); err != nil {
			return nil, fmt.Errorf("ftsbench: cold close: %w", err)
		}
	}
	return ds, nil
}

// timeSingleWrite writes one document and commits it, returning how long that
// took. The commit is what forces the pending document out to the shard, so
// the two cannot be separated on this path.
func timeSingleWrite(ui fts.UserIndex, mbox fts.MailboxRef, uid *uint32, tokens int) (time.Duration, error) {
	up, err := ui.BeginUpdate(mbox)
	if err != nil {
		return 0, fmt.Errorf("ftsbench: begin single: %w", err)
	}
	t0 := time.Now()
	if err := writeDoc(up, *uid, tokens); err != nil {
		return 0, err
	}
	if err := up.Commit(); err != nil {
		return 0, fmt.Errorf("ftsbench: commit single: %w", err)
	}
	*uid++
	return time.Since(t0), nil
}

func writeDoc(up fts.Update, uid uint32, tokens int) error {
	ok, err := up.SetBuildKey(fts.BuildKey{UID: uid, Type: fts.KeyBodyPart, ContentType: "text/plain"})
	if err != nil {
		return fmt.Errorf("ftsbench: set build key uid %d: %w", uid, err)
	}
	if !ok {
		return fmt.Errorf("ftsbench: engine refused the body part for uid %d", uid)
	}
	for t := 0; t < tokens; t++ {
		// Distinct terms per document: a corpus of one repeated token would
		// commit far cheaper than real mail, and cheap commits are exactly the
		// direction that would bias this measurement.
		term := "t" + strconv.Itoa(int(uid)) + "x" + strconv.Itoa(t)
		if err := up.BuildMore([]byte(term)); err != nil {
			return fmt.Errorf("ftsbench: build uid %d: %w", uid, err)
		}
	}
	return nil
}

func subtract(a, b []time.Duration) []time.Duration {
	out := make([]time.Duration, 0, len(a))
	for i := range a {
		d := a[i]
		if i < len(b) {
			d -= b[i]
		}
		if d < 0 {
			d = 0
		}
		out = append(out, d)
	}
	return out
}

func pct(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (len(s) * p) / 100
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
