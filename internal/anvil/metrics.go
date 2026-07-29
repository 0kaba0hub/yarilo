package anvil

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// requestSeconds is per-verb server-side latency. yarilo-anvil is a single
	// Deployment and its wire protocol is strict request/response per
	// connection, so a slow verb here serialises every login that needs it —
	// which makes this the metric that distinguishes "anvil is the queue" from
	// "anvil is fine" during a login stall.
	requestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "request_seconds",
		Help:      "Server-side latency of yarilo-anvil requests by verb and outcome.",
		Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 16), // 100µs … ~3s
	}, []string{"verb", "result"})

	sessions = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "sessions",
		Help:      "Current number of tracked login sessions.",
	})

	// sessionsReaped counts TTL evictions. A rising rate means sessions are
	// losing their heartbeat — either the login pod is starved or the TTL is
	// tighter than the heartbeat interval — and each reap makes the next
	// HEARTBEAT return reason=unknown to a session that is in fact alive.
	sessionsReaped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "sessions_reaped_total",
		Help:      "Total sessions dropped by the TTL sweeper.",
	})

	penaltyLookups = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "penalty_lookups_total",
		Help:      "Auth-penalty lookups by outcome (hit = a penalty was in force, miss = none).",
	}, []string{"result"})

	// connections is the saturation signal for a single-replica service: every
	// login currently opens its own connection, so this gauge tracks the login
	// rate until connection reuse lands.
	connections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "connections",
		Help:      "Currently open connections to yarilo-anvil.",
	})

	connectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "connections_total",
		Help:      "Total connections accepted by yarilo-anvil since start.",
	})
)

func observeRequest(verb, result string, start time.Time) {
	requestSeconds.WithLabelValues(verb, result).Observe(time.Since(start).Seconds())
}
