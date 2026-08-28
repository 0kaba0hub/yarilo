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

// observePart records one named part of a freshness check -- and only when a
// check is what is running. Replay is also reached from opening
// the map, where no whole is being timed: counting them there would let the
// parts exceed the total, and the unnamed remainder between them is the whole
// point of the split. A negative remainder says nothing.
func (m *Map) observePart(part string, d time.Duration) {
	if !m.inReload {
		return
	}
	metricMapReloadPart.WithLabelValues(part).Observe(d.Seconds())
}

// Metrics that attribute the cost of the mdbox map, which is the structure
// maildir and sdbox do not have (#1205). Four numbers, chosen so that between
// them they separate queueing for the lock service from work done under the
// lock, and the cost of writing from the slowing of reads.
var (
	// Acquiring the cross-process lock, and holding it, counted apart. The
	// first is a round trip to the lock service; the second is our own work
	// under the lock.
	//
	// Named for the round trip and not for waiting, because that is what it
	// times: the call is made, and paid for, when nothing holds the lock at
	// all. It was called a wait for three releases and was read as contention
	// every time -- 59-80x the hold, in every window ever measured, with the
	// summed holds of every process that takes this lock accounting for 2.5%
	// of it. Roughly 7 ms of the 8.8 is transport and scheduling; the lock
	// service answers in 1.5 (#1533).
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

	// Writers queue behind one another on the same in-process mutex, before
	// any of them reaches the lock service. Kept apart from the read wait
	// below rather than sharing one histogram with a label: reads and writes
	// are different populations, and merging them is the same conflation the
	// wait/hold split exists to avoid.
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

	// Opening the map is not a freshness check and was measured by nothing:
	// it reads the user's whole map index, replays the log and rebuilds the
	// UID index. It happens once per handle -- lazily, on the first touch of
	// storage -- so a workload that opens a session per operation pays it per
	// operation, and it would sit invisibly inside whichever command touched
	// storage first (#1205).
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

	// Freshness costs, split so the totals can be reconciled: two stats are
	// paid even on the fast path, replaying a sibling's tail is a read, and
	// rebuilding the UID index after it is neither. Whatever the total holds
	// beyond the three is a fourth cost nobody has named yet, and the gap is
	// the finding (#1205).
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

	// What a read costs on this driver, which is where the remaining gap to
	// maildir may live: maildir opens one file by name and is done, mdbox
	// resolves a map entry, opens a packed file and seeks inside it. Named
	// parts rather than one number, so the comparison says which step.
	//
	// lookup includes a freshness check when the map misses, so it overlaps
	// with the reload histograms above by design -- they answer "what does
	// staying fresh cost", this answers "what does a read cost".
	//
	// One Fetch can record "open" twice: a message flagged as living on the
	// alt tier is opened there first and falls back to the primary when that
	// fails. The sample count is therefore not the number of reads.
	//
	// "record" was called "body" until Fetch stopped reading the body into
	// memory (#1517). It timed io.ReadFull of the whole message then and it
	// times positioning on the record now -- reading its header and bounding
	// the body -- so the old name would have kept a series whose meaning had
	// changed underneath it. The body's own transfer is no longer timed here
	// at all: the caller reads as much of it as it wants, and how far that
	// goes is the caller's business.
	metricReadPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mdbox_read_part_seconds",
		Help:    "Time in one part of serving a message body: lookup, open or record.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"part"})
)
