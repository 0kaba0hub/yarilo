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

	// connectTotal makes the PR2 limit assertable by number rather than by
	// scanning cnt:* keys: result is ok, too_many_connections, state_error, or
	// bad_request. (request_seconds already carries the same _count, but a plain
	// counter keeps an acceptance gate a one-liner.)
	connectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "connect_total",
		Help:      "CONNECT outcomes by result.",
	}, []string{"result"})

	penaltyUpdates = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "penalty_updates_total",
		Help:      "Auth-penalty updates by effect (set = counter raised, clear = counter dropped).",
	}, []string{"result"})

	// kickEmitted / kickDelivered make the kick bus observable across replicas
	// (#908): EMIT increments kickEmitted on the pod that received the EMIT,
	// while the pod whose subscriber forwards the EVENT to its login client
	// increments kickDelivered. With Redis those are different pods, so a scrape
	// of both proves cross-replica delivery without racing a session death.
	kickEmitted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "kick_emitted_total",
		Help:      "Kick events published via EMIT.",
	})
	kickDelivered = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "kick_delivered_total",
		Help:      "Kick EVENT lines forwarded to a subscribed client.",
	})

	// redisErrors makes fail-open non-silent: a bounded Redis error that the
	// handler swallows (returning fail-open) still shows up here, labelled by op,
	// so a Redis outage is visible instead of only inferable from behaviour.
	redisErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "redis_errors_total",
		Help:      "Redis operation errors by op (fail-open is applied, but counted here).",
	}, []string{"op"})

	// reconcileAdjustments counts counter leaks corrected by Maintain — drift
	// that was previously invisible.
	reconcileAdjustments = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "anvil",
		Name:      "reconcile_adjustments_total",
		Help:      "Connection-counter leaks corrected by reconciliation.",
	})
)

func observeRequest(verb, result string, start time.Time) {
	requestSeconds.WithLabelValues(verb, result).Observe(time.Since(start).Seconds())
}

// redisErr records a fail-open Redis error for op and returns err unchanged, so
// call sites stay `return redisErr("op", err)`.
func redisErr(op string, err error) error {
	redisErrors.WithLabelValues(op).Inc()
	return err
}
