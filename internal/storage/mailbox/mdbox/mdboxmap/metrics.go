package mdboxmap

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ObserveReadPart records one part of serving a message body. Exported because
// the read path lives in the driver while the histogram belongs with the rest
// of the map's cost accounting.
func ObserveReadPart(part string, d time.Duration) {
	metricReadPart.WithLabelValues(part).Observe(d.Seconds())
}

// observePart records one part of a freshness check, and only while a check is
// what is running: replay is also reached from opening the map, where no whole
// is timed, and counting it there would let the parts exceed the total.
func (m *Map) observePart(part string, d time.Duration) {
	if !m.inReload {
		return
	}
	metricMapReloadPart.WithLabelValues(part).Observe(d.Seconds())
}

// The cost of the map, which is the structure maildir and sdbox do not have
// (#1205), split so queueing is separable from work and writing from reading.
var (
	// Acquire and hold, counted apart. Named for the round trip and not the
	// wait: it is paid when nothing holds the lock, and 7ms of the 8.8 is
	// transport (#1533).
	metricMapLockAcquire = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_lock_acquire_seconds",
		Help:    "Round trip to acquire the cross-process map lock, including any retries. Paid on every acquisition, contended or not.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10), // 100us .. ~26s
	})
	metricMapLockHold = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_lock_hold_seconds",
		Help:    "Time one map operation held the cross-process map lock, doing its work. Not observed where no lock service is configured.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
	})

	// Writers queue on the in-process mutex before any of them reaches the lock
	// service. Kept apart from the read wait: different populations, and merging
	// them is the conflation the wait/hold split exists to avoid.
	metricMapWriteBlocked = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_write_blocked_seconds",
		Help:    "Time a map write waited for the in-process map mutex, before reaching the lock service.",
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
	}, []string{"result"}) // fast | replay | fold | reopen
	metricMapReplayBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mdbox_map_replay_bytes_total",
		Help: "Bytes of append log replayed into the in-memory map.",
	})

	// Opening reads the whole index, replays the log and rebuilds the uid index,
	// once per handle -- so a session per operation pays it per operation.
	metricMapOpenSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_open_seconds",
		Help:    "Time to open the map for a handle, whole: reading the base index and replaying the log.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11), // 100us .. ~100s
	})
	metricMapOpenPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mdbox_map_open_part_seconds",
		Help:    "Time in one part of opening the map: base or replay.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11),
	}, []string{"part"})

	// Freshness costs, split so the totals reconcile: two stats even on the fast
	// path, the tail replay, and the uid-index rebuild. Whatever the total holds
	// beyond the three is an unnamed fourth cost, and the gap is the finding.
	metricMapReloadSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mdbox_map_reload_seconds",
		Help:    "Time in one map freshness check, whole.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11), // 10us .. ~10s
	})
	metricMapReloadPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mdbox_map_reload_part_seconds",
		Help:    "Time in one part of a map freshness check: stat or replay.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"part"})

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

	// What a read costs, by step. lookup overlaps the reload histograms by
	// design, and "open" can fire twice per Fetch, so its count is not reads.
	metricReadPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mdbox_read_part_seconds",
		Help:    "Time in one part of serving a message body: lookup, open or record.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"part"})
)
