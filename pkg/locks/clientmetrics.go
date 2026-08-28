package locks

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// clientBusyRetries counts the times a blocking acquisition found the resource
// held and slept before trying again.
//
// The lock service already counts refusals, but it counts them for the whole
// deployment: 5.5% of acquisitions there said nothing about which backend paid
// for them, or which resource. This counter sits where the sleeping happens, so
// contention is visible next to the acquisition latency it inflates (#1533).
//
// A retry is not the same as a failure: the acquisition that follows usually
// succeeds. What it measures is time spent waiting for somebody else, which is
// the only part of an acquisition that contention explains.
var clientBusyRetries = promauto.NewCounter(prometheus.CounterOpts{
	Name: "yarilo_locks_acquire_busy_retries_total",
	Help: "Blocking lock acquisitions that found the resource held and backed off before retrying.",
})
