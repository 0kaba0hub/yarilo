//go:build flatcurve

package ftsservice

import (
	"bytes"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// removeStoredMessages deletes the mail files under root, leaving the index
// entries behind: the shape of a mount that went away, and the only way to
// make a read fail without stubbing the storage.
func removeStoredMessages(t *testing.T, root string) int {
	t.Helper()
	removed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(path, "/cur/") || strings.Contains(path, "/new/") {
			if rerr := os.Remove(path); rerr == nil {
				removed++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return removed
}

// A message the indexer cannot read leaves a hole: its content is unsearchable
// until a rescan fills it. Nothing else reports that, so the skip is counted
// and logged at warning level -- a hole nobody can see is what let a whole
// class of failure live in Debug output (#1219).
func TestSkippedMessageIsCounted(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "goneawayzz")
	saveMessage(t, box, uidx, 2, "stillherezz")

	info, err := svc.opts.ResolveUser(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if n := removeStoredMessages(t, info.Home); n == 0 {
		t.Fatal("no message files were removed, so no read can fail")
	}

	// The level is part of the decision, not presentation: a skip at Debug is
	// invisible on an ordinary deployment, which is how the delivery-side gap
	// survived unnoticed for weeks (#1212).
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	before := testutil.ToFloat64(metricIndexSkipped.WithLabelValues("read"))
	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 2)

	if got := testutil.ToFloat64(metricIndexSkipped.WithLabelValues("read")); got <= before {
		t.Errorf("fts_index_skipped_total{reason=read} = %v, was %v: a hole was left with nothing to report it", got, before)
	}
	if !strings.Contains(logs.String(), "level=WARN msg=\"fts: message skipped") {
		t.Errorf("the skip was not reported at warning level:\n%s", logs.String())
	}

	// The run still finished: a message that cannot be read must not stop the
	// folder, which is the whole point of skipping rather than halting.
	last, _, err := svc.Status(testUser, testMbox)
	if err != nil {
		t.Fatal(err)
	}
	if last != 2 {
		t.Errorf("checkpoint = %d, want 2: an unreadable message stopped the run", last)
	}
}
