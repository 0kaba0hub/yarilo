package mdboxmap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics that attribute the cost of the mdbox map, which is the structure
// maildir and sdbox do not have (#1205). Four numbers, chosen so that between
// them they separate queueing for the lock service from work done under the
// lock, and the cost of writing from the slowing of reads.
var (
	// Waiting for the cross-process lock, and holding it, counted apart: the
	// first is somebody else's write, the second is our own work.
	metricMapLockWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_lock_wait_seconds",
		Help:    "Time one map operation waited for the cross-process map lock.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10), // 100us .. ~26s
	})
	metricMapLockHold = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_lock_hold_seconds",
		Help:    "Time one map operation held the map lock, doing its work.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
	})

	// A read takes no cross-process lock, but it takes the same in-process
	// mutex a writer holds while it waits. This is the number that decides
	// whether reads are paying for writes.
	metricMapReadBlocked = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_read_blocked_seconds",
		Help:    "Time a map read waited for the in-process map mutex.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
	})

	// Freshness checks: how often the two stats find nothing changed, against
	// how often a sibling's appends have to be replayed.
	metricMapReload = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mdbox_map_reload_total",
		Help: "Map freshness checks by outcome.",
	}, []string{"result"}) // fast | replay | reopen
	metricMapReplayBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mdbox_map_replay_bytes_total",
		Help: "Bytes of append log replayed into the in-memory map.",
	})

	// Compaction happens inside whichever append trips the threshold, so its
	// cost is a periodic stall rather than an even per-operation price.
	metricMapFlush = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mdbox_map_flush_total",
		Help: "Full base-index rewrites (map log compaction).",
	})
	metricMapFlushSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_flush_seconds",
		Help:    "Duration of one full base-index rewrite.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 9), // 1ms .. ~65s
	})
)
