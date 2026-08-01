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
	// metricIndexBuildHalts: hard buildmail failure halted a mailbox run
	// without advancing the checkpoint. Expected zero; a deterministic
	// failure keeps incrementing on every retry of the same UID.
	metricIndexBuildHalts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_index_build_halts_total",
		Help: "Mailbox index runs halted by a hard buildmail failure, without advancing the checkpoint past the failed message.",
	})
	metricQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fts_index_queue_depth",
		Help: "Pending FTS index jobs in the in-process queue.",
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
