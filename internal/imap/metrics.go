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
	Help:    "Server-side duration of one IMAP command, by command and storage driver. IDLE, NOTIFY and POLL wait on the client or the mailbox and are long by design.",
	Buckets: prometheus.ExponentialBuckets(0.0001, 4, 12), // 100us .. ~7min
}, []string{"command", "driver"})
