package imap_test

import (
	"sync"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// slowFTS answers Status with nothing indexed until after a delay, then reports
// the mailbox indexed. That is a cold account: the indexer has the job and has
// not reached it yet, which for the first few seconds looks exactly like a
// broken backend.
type slowFTS struct {
	mu       sync.Mutex
	queuedAt time.Time
	delay    time.Duration
	maxUID   uint32
	prepends int
	// neverStarts models the actual broken backend: the job is taken and the
	// checkpoint never moves.
	neverStarts bool
}

func (f *slowFTS) Index(string, fts.MailboxRef, uint32, int) error { return nil }

func (f *slowFTS) Prepend(_ string, _ fts.MailboxRef, maxUID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepends++
	f.queuedAt = time.Now()
	f.maxUID = maxUID
	return nil
}

func (f *slowFTS) Expunge(string, fts.MailboxRef, uint32) error { return nil }

// Lookup answers as an engine that has the mailbox indexed would: the one
// message is a definite hit. A fake that matched nothing would let a refusal
// and a successful-but-empty search look the same.
func (f *slowFTS) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{Definite: []uint32{1}}, nil
}

func (f *slowFTS) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.neverStarts || f.queuedAt.IsZero() || time.Since(f.queuedAt) < f.delay {
		return 0, 1, nil // nothing indexed yet
	}
	return f.maxUID, 1, nil
}

func (f *slowFTS) Rescan(string, fts.MailboxRef) error { return nil }
func (f *slowFTS) Optimize(string) error               { return nil }
func (f *slowFTS) Close() error                        { return nil }

// TestSearchOnColdAccountWaitsForTheFirstIndex is #1379: the first touch of an
// account nobody has used for a while was refused with "Mailbox is still being
// indexed, try again later", and the immediate retry succeeded. The stall
// detector gave up after ~2s of a flat checkpoint -- but a mailbox with nothing
// indexed has no baseline for that judgement, so it was measuring the indexer's
// queue latency, not its health.
//
// The delay here is longer than the stall budget and shorter than the timeout:
// the exact window the old code refused in.
func TestSearchOnColdAccountWaitsForTheFirstIndex(t *testing.T) {
	fake := &slowFTS{delay: 3 * time.Second}
	c := startFTSTestServerWith(t, fake, false, t.TempDir(), func(o *imapserver.FTSOptions) {
		o.ReadFallback = false // the sandbox's setting: no silent slow scan
		o.Timeout = 10 * time.Second
	})
	appendBody(t, c, "coldstartbody")

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if got := searchBody(t, c, "coldstartbody"); len(got) != 1 {
		t.Errorf("search returned %v, want the one message", got)
	}
}

// TestSearchGivesUpOnAnIndexerThatNeverStarts is the other half, and the reason
// the stall detector exists: with the checkpoint flat and the backend broken,
// the client must not be held to the full timeout.
func TestSearchGivesUpOnAnIndexerThatNeverStarts(t *testing.T) {
	fake := &slowFTS{neverStarts: true}
	c := startFTSTestServerWith(t, fake, false, t.TempDir(), func(o *imapserver.FTSOptions) {
		o.ReadFallback = true // fall back rather than refuse, so we can time it
		o.Timeout = 30 * time.Second
	})
	appendBody(t, c, "stuckbody")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	searchBody(t, c, "stuckbody")
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("a broken indexer held the client for %v: the early exit is gone", elapsed)
	}
}
