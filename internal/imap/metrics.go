package imap

import (
	"errors"
	"io/fs"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricCommandSeconds times a client command end to end, on the server side.
//
// It exists because everything measured below it -- storage reads, map
// freshness, saves -- added up to a few per cent of a run whose throughput
// differed twofold between drivers. Parts cannot explain a whole nobody has
// measured, so this is the whole: what is left after subtracting the named
// parts is the cost still unaccounted for, and that remainder is the finding.
//
// IDLE, NOTIFY and POLL are long by design -- they wait for the mailbox to
// change -- so they say nothing about work done and should be read apart from
// the rest.
var metricCommandSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "imap_command_seconds",
	Help:    "Server-side duration of one IMAP command, by command and storage driver. IDLE, NOTIFY and POLL wait on the client or the mailbox and are long by design; APPEND includes reading the message literal from the client, so a slow client shows up here as slow storage.",
	Buckets: commandBuckets,
}, []string{"command", "driver"})

// commandBuckets is fine where the commands are and coarse where they are not.
//
// A single exponential series stepping by four put 6.4ms and 25.6ms in
// neighbouring boundaries, so every command between them -- which is most of
// them -- landed in one bucket, p50 and p99 alike. Asked to compare THREAD
// (10.3ms) against SEARCH (4.5ms) it answered "both under 25.6ms": true, and
// no help. A histogram that cannot separate two commands differing by 2x
// cannot answer the question this one is read for (#1462).
//
// Doubling between 1ms and 128ms buys that resolution; the decade below and
// the long tail above stay coarse, because nobody reads a quantile of IDLE.
// Note the average (sum/count) was always exact -- it is the quantiles this
// changes, and quantiles are what a tail investigation needs.
var commandBuckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.002, 0.004, 0.008, 0.016, 0.032, 0.064, 0.128,
	0.25, 0.5, 1, 5, 30, 120, 600,
}

// The maildir proactive scan, as a whole and as a decision.
//
// It is the one thing maildir does on SELECT that the index-authoritative
// drivers do not: walk cur/ and new/ so a message delivered by an MDA or moved
// by a second MUA appears without an operator rebuild. That is a correctness
// property, not waste — but it is also the named suspect for maildir's SELECT
// being the most expensive of the three drivers (#1265), and a suspect without
// a number is an opinion.
//
// The skipped count is the interesting half. The scan is already gated on a
// change token, so in principle a folder nobody touched costs a stat and
// nothing else; if skips stay at zero under a workload that re-selects
// unchanged folders, the gate is not reaching the case it was built for.
var (
	metricMaildirSyncSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "imap_maildir_sync_seconds",
		Help:    "Time one maildir proactive reconcile took, from computing the change token through the index update.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11), // 100us .. ~100s
	})
	metricMaildirSync = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_maildir_sync_total",
		Help: "Maildir reconcile decisions: scanned means cur/ and new/ were walked, skipped means the change token said nothing had changed.",
	}, []string{"result"}) // scanned | skipped
)

// metricUnreadable counts messages a command answered short because it could
// not read them. The event is one event -- the server answered with less than
// it knows, and nothing in the answer says so (#1283) -- so it is one series
// with the command as a label. An alert on it is one expression, not a sum of
// series that someone must remember to extend when a third command starts
// scanning.
//
// FETCH counts here too, and did not until #1532. A message whose record the
// driver cannot read is answered as `* n FETCH ()` -- present in the mailbox,
// counted by SELECT, its size served from the index, and every content section
// empty. That is indistinguishable to a client from an empty message, and it
// is what both format faults we have ever had looked like: this counter stayed
// at zero through both of them.
var metricUnreadable = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "imap_unreadable_messages_total",
	Help: "Messages a command could not read, and therefore silently left out of its answer. reason=gone is a message the index lists and the store does not; reason=unreadable is a message that is there and could not be read.",
}, []string{"command", "reason"})

// Reasons a message could not be read. They are kept apart because only one of
// them means something is wrong with the stored mail.
//
// gone: the file is not there. One connection expunging while another fetches
// from an index snapshot taken before it produces exactly this, and a clean
// gate produced 239 of them (#1538). Counted rather than dropped, because the
// same shape is also index/store divergence, which is worth seeing -- but it
// cannot share a series with the other one, or the alert fires on ordinary
// traffic and gets turned off.
//
// unreadable: the file is there and its contents could not be served. This is
// the one that caught nothing during #1525 because FETCH did not count at all.
const (
	reasonGone       = "gone"
	reasonUnreadable = "unreadable"
)

// unreadableReason classifies a driver read error. Drivers wrap a vanished file
// with their own corruption sentinel, deliberately -- an index that still
// references it has diverged from the store -- so the classification looks
// underneath for the original os.ErrNotExist rather than at the sentinel.
func unreadableReason(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return reasonGone
	}
	return reasonUnreadable
}
