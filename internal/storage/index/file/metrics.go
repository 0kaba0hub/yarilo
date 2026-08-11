package file

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What a read command costs inside the index, as a whole and in named parts.
//
// The folder index is the one layer maildir and sdbox share, which is where the
// 24-32% gap to the reference has to live: the two drivers have nothing else in
// common below the session (#1229). None of it was measured — the lock path
// logged at debug level and nothing else was timed at all — so a per-command
// number could say a command costs 25ms without saying whether that is the
// round trip to the lock service, the freshness check, or rebuilding the
// folder's message list for the tenth time in a row.
//
// The parts are observed only inside the whole, and what the whole holds beyond
// them is the finding. It cannot be negative, and a test asserts it.
var (
	// A read that fails before it builds its answer reports the parts it
	// reached and not the ones it did not, so the counts of the parts are not
	// equal to each other or to the count of the whole. The sums still
	// reconcile, which is what the split is for.
	metricReadSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fileindex_read_seconds",
		Help:    "Time one read of a folder's state took, whole: lock, freshness check and building the answer.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11), // 10us .. ~10s
	})
	metricReadPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fileindex_read_part_seconds",
		Help:    "Time in one part of a folder read: lock, reload or build.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"part"})

	// The lock is its own question as well as a part of the read: it is the one
	// place a read leaves the process. The reference takes a local fcntl here.
	metricLockWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fileindex_lock_wait_seconds",
		Help:    "Time an index operation waited for the cross-process folder lock, by mode and by which path took it.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10), // 100us .. ~26s
	}, []string{"mode", "site"})
	metricLockRelease = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fileindex_lock_release_seconds",
		Help:    "Time releasing the cross-process folder lock, by mode and site. The second round trip an operation makes, and about as expensive as the first.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
	}, []string{"mode", "site"})
	metricLockAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fileindex_lock_acquired_total",
		Help: "Cross-process folder locks acquired, by mode and by which path took it. Each acquisition is followed by a release, so an operation that takes the lock makes two round trips to the lock service.",
	}, []string{"mode", "site"}) // shared | exclusive × open-probe | reload-fallback | read | write
	metricReload = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fileindex_reload_total",
		Help: "Folder freshness checks by outcome: adopt means a rewritten base was proven to hold what memory already held and its records were not read.",
	}, []string{"result"}) // adopt
	metricLineageStamped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fileindex_lineage_stamped_total",
		Help: "Folder indexes given a lineage on first open because they were written before the extension. Expected to rise once per folder after an upgrade and stay flat afterwards; two pods racing the same first open can each stamp it, so a folder may count twice.",
	})
	metricLockReentrant = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fileindex_lock_reentrant_total",
		Help: "Index operations that already held the folder lock, by mode and site. No round trip was made.",
	}, []string{"mode", "site"})
)

// The sites a folder lock can be taken from. Named rather than free-form so
// "where do the remaining acquisitions come from" has a finite answer, and so a
// new call site has to choose one deliberately.
//
// The distinction that matters most is open-probe against reload-fallback:
// the first is a folder being opened, which is legitimate and bounded by
// sessions; the second is a read that wanted the lock-free path and could not
// prove freshness, which means the lineage never arrived and the migration did
// not do its job.
const (
	lockSiteOpenProbe = "open-probe"      // opening or repairing a folder
	lockSiteFallback  = "reload-fallback" // an unlocked read with nothing to prove freshness with
	lockSiteRead      = "read"            // a read that is locked on purpose: its answer decides a write
	lockSiteWrite     = "write"           // a mutation
)

// lockMode names the label so a caller cannot pass "true" and mean shared.
func lockMode(shared bool) string {
	if shared {
		return "shared"
	}
	return "exclusive"
}

// observeReadPart records one named part of a read. Guarded by the caller
// knowing it is inside a read: the same functions are reached from write paths,
// where there is no whole for a part to sit inside, and a part counted outside
// its whole makes the remainder go negative and say nothing.
func observeReadPart(part string, d time.Duration) {
	metricReadPart.WithLabelValues(part).Observe(d.Seconds())
}
