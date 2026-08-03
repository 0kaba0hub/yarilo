package ftsservice

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FTS metrics, registered on the default registry. Cardinality is bounded:
// no per-user/per-mailbox labels, never query terms (private content).
var (
	metricIndexMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_messages_total",
		Help: "Messages indexed by the FTS worker.",
	})
	metricIndexDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_index_duration_seconds",
		Help:    "Wall-clock duration of one FTS index job.",
		Buckets: prometheus.DefBuckets,
	})
	metricIndexErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_errors_total",
		Help: "FTS index jobs that returned an error.",
	})
	// metricQueueMerged: a request that folded into a pass already queued for
	// the same mailbox. Growth here is duplicate work that never happened —
	// and, because a duplicate would have taken the lock before discovering it
	// had nothing to do, contention that never happened either.
	metricQueueMerged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_queue_merged_total",
		Help: "Index requests merged into a pass already queued for that mailbox.",
	})
	// metricQueueRequeued: a mailbox put back because a request arrived while
	// its pass was running. That request's messages are above the checkpoint the
	// running pass had already read, so without this they would wait for an
	// unrelated event to queue the mailbox again.
	metricQueueRequeued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_queue_requeued_total",
		Help: "Mailboxes re-queued because a request arrived mid-pass.",
	})
	// metricWorkersBusy: index workers currently running a pass. Reads at most
	// fts_index_workers; a value stuck below it while the queue is deep is the
	// symptom metricPopSkipped explains.
	metricWorkersBusy = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fts_workers_busy",
		Help: "Index workers currently running a pass.",
	})
	// metricPopSkipped: queued mailboxes a worker passed over because their
	// user was already being indexed. Without it, "workers idle while the queue
	// is deep" looks identical to a stall, when it is the dispatcher correctly
	// refusing work that would only serialise inside the engine's per-user
	// mutex.
	metricPopSkipped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_pop_skipped_total",
		Help: "Queued mailboxes skipped because their user was already being indexed.",
	})
	// metricQueueWait: push to pop. Distinguishes "the queue is deep" from
	// "the queue is deep and nothing is draining".
	metricQueueWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_queue_wait_seconds",
		Help:    "Time an index request waited in the queue before a worker took it.",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 5, 15, 60, 300},
	})
	// metricFetch / metricBuild: where a pass actually spends its time. Fetch
	// dominating means the win is in overlapping reads; Build dominating means
	// it is in more workers. Without the split, choosing between them is a
	// guess.
	metricFetch = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_fetch_seconds",
		Help:    "Time reading one message from storage during indexing.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	})
	metricBuild = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_build_seconds",
		Help:    "Time parsing and tokenising one message during indexing.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	})
	// metricMessageBytes: the two histograms above are uninterpretable without
	// it — 200ms on 30MB and 200ms on 3KB are different diagnoses.
	metricMessageBytes = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_message_bytes",
		Help:    "Size of messages fed to the indexer.",
		Buckets: prometheus.ExponentialBuckets(1024, 4, 8),
	})
	// metricLagUIDs / metricLagSeconds: what a user actually feels — "search
	// does not find recent mail". Every other metric here is a proxy for these
	// two, and the plan succeeds only if they fall.
	metricLagUIDs = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fts_index_lag_uids",
		Help: "Largest gap between a mailbox's highest UID and its index checkpoint.",
	})
	metricLagSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fts_index_lag_seconds",
		Help: "Age of the oldest message not yet indexed.",
	})
	// metricIndexDeferred: a pass that could not take the mailbox lock and was
	// requeued. Steady growth means write contention, not breakage — it is the
	// signal that indexing is lagging behind delivery rather than failing.
	metricIndexDeferred = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_deferred_total",
		Help: "FTS index passes requeued after losing the mailbox lock.",
	})
	// metricIndexDropped: a pass abandoned after exhausting its retries. Every
	// increment is mail that is not in the index and will not be until
	// something else queues that mailbox. Expected zero.
	metricIndexDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_dropped_total",
		Help: "FTS index passes given up on after repeated lock contention.",
	})
	// metricIndexBuildHalts: hard buildmail failure halted a mailbox run
	// without advancing the checkpoint. Expected zero; a deterministic
	// failure keeps incrementing on every retry of the same UID.
	metricIndexBuildHalts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_build_halts_total",
		Help: "Mailbox index runs halted by a hard buildmail failure, without advancing the checkpoint past the failed message.",
	})
	metricQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fts_index_queue_depth",
		Help: "Mailboxes waiting for an index pass. A running pass is not pending, and one mailbox counts once however many requests it received.",
	})
	metricLookupTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_lookup_total",
		Help: "FTS LOOKUP requests served.",
	})
	metricLookupErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_lookup_errors_total",
		Help: "FTS LOOKUP requests that returned an error.",
	})
	metricLookupDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_lookup_duration_seconds",
		Help:    "Duration of one FTS LOOKUP.",
		Buckets: prometheus.DefBuckets,
	})
	metricLookupCandidates = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_lookup_candidates",
		Help:    "Candidate UIDs (definite+maybe) returned by one FTS LOOKUP.",
		Buckets: []float64{0, 1, 5, 10, 50, 100, 500, 1000, 5000},
	})
	metricRecoveryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fts_recovery_total",
		Help: "FTS engine recoveries (a broken/closed index handle was evicted for reopen).",
	}, []string{"reason"})
	// metricLockWait: per-mailbox FTS write-lock acquire time — the direct
	// signal of cross-pod contention.
	metricLockWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fts_lock_wait_seconds",
		Help:    "Time spent acquiring the per-mailbox FTS write lock.",
		Buckets: prometheus.DefBuckets,
	})
)

// ObserveLockWait records how long acquiring the per-mailbox FTS write lock
// took. Called by the binary's LockMailbox wrapper.
func ObserveLockWait(d time.Duration) { metricLockWait.Observe(d.Seconds()) }
