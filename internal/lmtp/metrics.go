package lmtp

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricDeliverySeconds is one recipient's delivery, end to end: from the point
// the message is in hand to the status this server reports for that recipient.
//
// Whole path, not one seam of it. The promise a delivery metric makes is that
// a regression shows up on a graph rather than in a complaint, and the next
// regression may have nothing to do with the seam that prompted this one.
//
// Rejections are timed too, under their own outcome: a mailbox that rejects
// slowly is still a mailbox that answers slowly, and a quota check that walks
// every folder is exactly the kind of cost that hides when only the happy path
// is measured.
//
// Buckets start at half a millisecond because a sandbox delivery is single
// milliseconds; the tail reaches minutes because a delivery waiting on a
// mailbox lock does.
var metricDeliverySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "lmtp_delivery_seconds",
	Help:    "Duration of one LMTP recipient delivery, from message in hand to the status reported for that recipient.",
	Buckets: deliveryBuckets,
}, []string{"outcome"})

// deliveryBuckets double where deliveries actually land (4-256ms here) and
// stay coarse either side.
//
// Stepping by three put 40.5ms and 121.5ms next to each other, so the whole
// band a delivery lives in was one answer. The first question anyone asks of
// this metric is about the SHAPE of the drift -- is the median rising or is
// the tail? -- and a bucket that wide cannot tell those apart (#1462).
var deliveryBuckets = []float64{
	0.0005, 0.001, 0.002,
	0.004, 0.008, 0.016, 0.032, 0.064, 0.128, 0.256,
	0.5, 1, 5, 30, 120,
}

// metricThreadRecordSeconds is the threading sidecar write alone, inside the
// delivery above.
//
// It answers the question the whole threading QA window asks -- how much of a
// delivery is the conversation -- without an A/B rollout, and in production it
// separates "delivery got slower because of threading" from "delivery got
// slower". Its own series rather than a stage label on the delivery histogram:
// a label shares the buckets, and this measures something two orders of
// magnitude smaller.
var metricThreadRecordSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "lmtp_thread_record_seconds",
	Help:    "Duration of the threading sidecar write within an LMTP delivery, lock included.",
	Buckets: threadRecordBuckets,
})

// threadRecordBuckets double across 0.25-4ms, which is where the sidecar write
// measured: ~1ms, flat across drivers and account sizes. A metric whose whole
// job is to say whether that figure moves needs to resolve a doubling of it,
// not a tripling.
var threadRecordBuckets = []float64{
	0.000125, 0.00025, 0.0005, 0.001, 0.002, 0.004,
	0.008, 0.016, 0.064, 0.256, 1, 5,
}

// observeDelivery records one recipient's outcome. Rejections carry a 4xx/5xx
// status, so the label is decided by what the recipient is told, not by
// whether the code took an early exit.
func observeDelivery(start time.Time, err error) {
	outcome := "delivered"
	if err != nil {
		outcome = "rejected"
	}
	metricDeliverySeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// setStatus reports one recipient's outcome and times it. Every exit from the
// per-recipient path goes through here, which is the point: a path that
// reports a status without timing it is a delivery the graph does not know
// happened, and rejections are the ones most likely to be forgotten.
func setStatus(status statusCollector, rcpt string, start time.Time, err error) {
	observeDelivery(start, err)
	status.SetStatus(rcpt, err)
}

// setProxyStatus reports a proxied recipient's outcome, deliberately WITHOUT
// timing it.
//
// Proxied recipients are answered from one fan-out that covers all of them, so
// there is no per-recipient duration to observe: timing here would record the
// same wall clock once per recipient and call it N deliveries. That seam wants
// its own measurement of the fan-out, which is a different question from what
// a local delivery costs -- and until it has one, this function exists so the
// exception is a named function rather than a bare call the guard has to
// forgive.
func setProxyStatus(status statusCollector, rcpt string, err error) {
	status.SetStatus(rcpt, err)
}

// statusCollector is go-smtp's StatusCollector, named here so the timing
// helper does not drag the SMTP package into this file.
type statusCollector interface {
	SetStatus(rcptTo string, err error)
}
