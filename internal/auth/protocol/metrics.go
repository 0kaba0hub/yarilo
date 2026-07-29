package protocol

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metric buckets reach into the tens of seconds on purpose. The AUTH path
// deliberately sleeps — auth-penalty tarpit, policy tarpit, and the
// timing-leak failure delay — so a client-observed AUTH can legitimately take
// seconds. A 400ms ceiling (as used for lock acquisition) would clip exactly
// the observations that matter when diagnosing a login stall.
var authLatencyBuckets = prometheus.ExponentialBuckets(0.001, 2, 16) // 1ms … ~65s

var (
	// requestSeconds is wall-clock per verb as the client experiences it,
	// INCLUDING the deliberate delays above. That is the intended semantic:
	// the question this metric answers is "how long did the login proxy wait
	// on yarilo-auth", not "how much work did auth do". Compare against
	// passdbSeconds to separate real backend cost from intentional tarpit.
	requestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "request_seconds",
		Help:      "Wall-clock latency of yarilo-auth requests by verb, including deliberate penalty/policy/failure delays.",
		Buckets:   authLatencyBuckets,
	}, []string{"verb", "result"})

	// cacheLookups splits what Cache.Lookup collapses into a single miss
	// counter. The taxonomy matters operationally: `expired` means the TTL is
	// too short for the login rate, `pwd_mismatch` means a stale credential is
	// being retried, and both are invisible if reported as plain misses.
	cacheLookups = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "cache_lookups_total",
		Help:      "Auth cache lookups by outcome (hit, miss, expired, pwd_mismatch).",
	}, []string{"result"})

	cacheEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "cache_entries",
		Help:      "Current number of entries held in the auth cache.",
	})

	// cacheBytes against the configured cap is what predicts eviction storms:
	// the cache is bytes-bounded, so a full cache silently degrades into a
	// pass-through and every login pays the passdb round-trip again.
	cacheBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "cache_bytes",
		Help:      "Current payload weight of the auth cache in bytes.",
	})

	cacheMaxBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "cache_max_bytes",
		Help:      "Configured byte cap of the auth cache (0 = caching disabled).",
	})

	// passdbSeconds isolates one chain driver's cost — the SQL round-trip and
	// the password-scheme verification it performs internally. Labelled by
	// driver so a slow MySQL is distinguishable from a slow bcrypt.
	passdbSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "passdb_seconds",
		Help:      "Latency of a single passdb chain driver call, by driver and outcome.",
		Buckets:   prometheus.ExponentialBuckets(0.0005, 2, 15), // 0.5ms … ~16s
	}, []string{"driver", "result"})

	// connections / connectionsTotal make per-login connection churn visible.
	// This is the signal #878 hypothesised about and nobody could measure:
	// connectionsTotal rising in lockstep with the login rate means every
	// login pays a fresh mTLS handshake; a flat curve means connections are
	// being reused.
	connections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "connections",
		Help:      "Currently open client-protocol connections to yarilo-auth.",
	})

	connectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "connections_total",
		Help:      "Total client-protocol connections accepted by yarilo-auth since start.",
	})
)

// DriverName is the optional interface a Passdb implements to label its own
// metrics. Drivers that do not implement it are reported as "unknown" — the
// Passdb contract stays a single Authenticate method so external and test
// implementations keep compiling.
type DriverName interface {
	DriverName() string
}

func driverLabel(db Passdb) string {
	if n, ok := db.(DriverName); ok {
		if s := n.DriverName(); s != "" {
			return s
		}
	}
	return "unknown"
}

func observeRequest(verb, result string, start time.Time) {
	requestSeconds.WithLabelValues(verb, result).Observe(time.Since(start).Seconds())
}

func observePassdb(driver, result string, start time.Time) {
	passdbSeconds.WithLabelValues(driver, result).Observe(time.Since(start).Seconds())
}

// resultLabel maps a chain Result onto the fixed label set. Kept separate from
// authResultFromChain so label names stay stable even if the wire mapping
// changes.
func resultLabel(r Result, err error) string {
	if err != nil {
		return "error"
	}
	switch r {
	case ResultOK:
		return "ok"
	case ResultTempFail:
		return "tempfail"
	case ResultNext:
		return "next"
	default:
		return "fail"
	}
}
