package locks

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// clientBusyRetries counts blocking acquisitions that found the resource held
// and slept before trying again. Counted here rather than at the service, which
// counts refusals for the whole deployment and names neither the backend that
// paid nor the resource (#1533). A retry is not a failure: what it measures is
// time spent waiting for somebody else. The label is the key's own prefix, so
// no new plumbing carries it (#1535).
var clientBusyRetries = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "yarilo_locks_acquire_busy_retries_total",
	Help: "Blocking lock acquisitions that found the resource held and backed off before retrying, by resource class.",
}, []string{"resource"})

// resourceClasses is every prefix the constructors in resources.go produce.
//
// A closed set on purpose: the label comes from a key that also carries a
// username and a folder name, and an unrecognised prefix must not become a
// label value. One malformed key would otherwise put a user's address into a
// metric and make the series unbounded.
var resourceClasses = map[string]struct{}{
	"idx":      {},
	"mdboxmap": {},
	"mbox":     {},
	"fts":      {},
	"mlist":    {},
	"deliver":  {},
	"sieve":    {},
	"threads":  {},
	"subs":     {},
	"acllist":  {},
}

// resourceClass returns the prefix of a lock key when it is one this code
// produces, and "other" otherwise.
//
// A key with no colon is handled by the set alone: Cut returns the whole string
// as the prefix, which is not in the set. An explicit check for the separator
// would be a branch no input can reach.
func resourceClass(resource string) string {
	prefix, _, _ := strings.Cut(resource, ":")
	if _, known := resourceClasses[prefix]; !known {
		return "other"
	}
	return prefix
}

// What a wait costs, measured once per acquisition: clientBusyRetries counts
// the draws lost, these say what losing them cost (#1640).
var (
	clientAcquireWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "yarilo_locks_acquire_wait_seconds",
		Help:    "Time from the first attempt at a blocking acquisition to the lock being taken or the wait being abandoned, by resource class. One observation per acquisition, not per attempt.",
		Buckets: prometheus.ExponentialBuckets(0.001, 3, 11), // 1ms … ~59s
	}, []string{"resource"})
	clientAcquireAttempts = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "yarilo_locks_acquire_attempts",
		Help:    "Attempts a blocking acquisition made before the lock was taken, by resource class. 1 means it was free.",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256},
	}, []string{"resource"})
	clientGaveUp = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "yarilo_locks_acquire_gave_up_total",
		Help: "Blocking acquisitions abandoned because the caller's deadline passed, by resource class and by how many attempts they had made. These are the stalls, counted where they happen.",
	}, []string{"resource", "attempts"})
)

// attemptBucket names a range: an unbounded attempt count on a label would make
// one series per contender.
func attemptBucket(n int) string {
	switch {
	case n <= 1:
		return "1"
	case n <= 3:
		return "2-3"
	case n <= 7:
		return "4-7"
	case n <= 15:
		return "8-15"
	case n <= 31:
		return "16-31"
	case n <= 63:
		return "32-63"
	}
	return "64+"
}

// Where an acquisition's time goes inside the call that hides it: queueing for a
// pool connection, against the exchange with the service (#1650).
var clientPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "yarilo_locks_client_part_seconds",
	Help:    "Time in one part of a command: waiting for a pool connection, or the exchange with the lock service on it.",
	Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
}, []string{"part"})
