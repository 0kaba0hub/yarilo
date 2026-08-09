// Package mailboxmetrics holds the timings shared by the mailbox drivers.
//
// One metric name per question, with the driver as a label, because the
// question these answer is comparative: what a save costs on mdbox is only
// meaningful beside what it costs on maildir. Two names would make that a
// join, and a join is where a comparison quietly stops being made.
package mailboxmetrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	saveSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mailbox_save_seconds",
		Help:    "Time to store one message, whole, by driver.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11), // 10us .. ~10s
	}, []string{"driver"})

	savePartSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mailbox_save_part_seconds",
		Help:    "Time in one named part of storing a message. The parts sum to no more than the whole; what is left over is a cost nobody has named yet.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"driver", "part"})
)

// ObserveSave records one whole save.
func ObserveSave(driver string, d time.Duration) {
	saveSeconds.WithLabelValues(driver).Observe(d.Seconds())
}

// ObserveSavePart records one named step of a save. A driver that does not
// have a given step simply never reports it.
func ObserveSavePart(driver, part string, d time.Duration) {
	savePartSeconds.WithLabelValues(driver, part).Observe(d.Seconds())
}

// TimeSavePart runs fn and records how long it took.
func TimeSavePart(driver, part string, fn func() error) error {
	start := time.Now()
	err := fn()
	ObserveSavePart(driver, part, time.Since(start))
	return err
}
