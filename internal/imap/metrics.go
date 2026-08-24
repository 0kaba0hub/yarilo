package imap

import (
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
	Buckets: prometheus.ExponentialBuckets(0.0001, 4, 12), // 100us .. ~7min
}, []string{"command", "driver"})

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

// metricUnreadable counts messages a command could not read while scanning.
// The event is one event -- the server answered with less than it knows, and
// nothing in the answer says so (#1283) -- so it is one series with the
// command as a label. An alert on it is one expression, not a sum of series
// that someone must remember to extend when a third command starts scanning.
var metricUnreadable = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "imap_unreadable_messages_total",
	Help: "Messages a command's scan could not read, and therefore silently left out of its answer.",
}, []string{"command"})
