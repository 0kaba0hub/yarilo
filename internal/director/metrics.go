package director

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// backendInfo carries per-backend state. Value is always 1; status label
	// carries the state: "up" | "flush".
	backendInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "backend_info",
		Help:      "Backend ring membership and status (1 = present, status label = up|flush).",
	}, []string{"ip", "port", "tag", "status"})

	// backendSessions is the current number of active proxied sessions per
	// backend IP (summed across all protocols), sourced from the SESSION-OPEN/
	// SESSION-CLOSE registry (sessByBE). Each director exposes the sessions of
	// the login pods connected to IT — a session is reported to exactly one
	// director, so sum across directors (sum by (ip)) for the cluster total.
	backendSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "backend_sessions",
		Help:      "Current active proxied sessions per backend IP (all protocols); sum across directors for the total.",
	}, []string{"ip", "tag"})

	// ringEventUnknown counts ring events this build does not know. It is the
	// signal that a peer is speaking a vocabulary this member has not learned
	// -- a rollout in progress, or one that skipped a step. Labelled by kind
	// so the answer to "which event am I losing" needs no log grep.
	ringEventUnknown = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "ring_event_unknown_total",
		Help:      "Ring events dropped because this build does not know the kind (label: kind).",
	}, []string{"kind"})

	// joinAccepted / joinRejected count ring DIRECTOR-JOIN outcomes (#750).
	// Rejected includes: no ring secret configured, malformed proof, and
	// HMAC mismatch — the counter alone doesn't distinguish which; the
	// accompanying slog.Warn does.
	joinAccepted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "ring_join_accepted_total",
		Help:      "Total number of ring DIRECTOR-JOIN requests accepted.",
	})
	joinRejected = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "ring_join_rejected_total",
		Help:      "Total number of ring DIRECTOR-JOIN requests rejected (no secret configured, malformed or invalid HMAC proof).",
	})

	// lookupSeconds is the login-blocking path: a login proxy cannot route
	// until LOOKUP answers. The result label separates a healthy assignment
	// from the two states that make a login retry — `killing` (confirmed-kick
	// hold, retryable by design) and `no_backends`.
	lookupSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "lookup_seconds",
		Help:      "Server-side latency of director LOOKUP by outcome (sticky, assigned, killing, no_backends, bad_request).",
		Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 16), // 100µs … ~3s
	}, []string{"result"})
)

func observeLookup(result string, start time.Time) {
	lookupSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// updateMetrics refreshes all backend Prometheus gauges. Called after every
// ring mutation and on every session open/close (backend_sessions tracks the
// live SESSION-OPEN/SESSION-CLOSE registry).
func (s *Server) updateMetrics() {
	backends := s.ring.Backends()

	// Current active sessions per backend IP, from the SESSION-OPEN/CLOSE
	// registry (this director's view). len(set) = sessions on that backend.
	sessCounts := s.backendSessionCounts()

	// Clear old series so removed backends don't linger.
	backendInfo.Reset()
	backendSessions.Reset()

	for _, b := range backends {
		portStr := strconv.Itoa(b.Port)
		status := "up"
		if !b.Up {
			status = "flush"
		}
		backendInfo.WithLabelValues(b.IP, portStr, b.Tag, status).Set(1)
		backendSessions.WithLabelValues(b.IP, b.Tag).Set(float64(sessCounts[b.IP]))
	}
}

// backendSessionCounts returns the current active-session count per backend IP
// from the SESSION-OPEN/CLOSE registry (sessByBE), summed across all protocols.
func (s *Server) backendSessionCounts() map[string]int {
	s.sessRecMu.RLock()
	defer s.sessRecMu.RUnlock()
	out := make(map[string]int, len(s.sessByBE))
	for ip, set := range s.sessByBE {
		out[ip] = len(set)
	}
	return out
}

// sessionCounts returns, per backend IP, the total active sessions and the
// per-protocol breakdown (#797 least_sessions placement). Both from the live
// SESSION-OPEN/CLOSE registry.
func (s *Server) sessionCounts() (total map[string]int, byProto map[string]map[string]int) {
	s.sessRecMu.RLock()
	defer s.sessRecMu.RUnlock()
	total = make(map[string]int, len(s.sessByBE))
	byProto = make(map[string]map[string]int, len(s.sessByBE))
	for ip, set := range s.sessByBE {
		total[ip] = len(set)
	}
	for _, rec := range s.sessById {
		if byProto[rec.backend] == nil {
			byProto[rec.backend] = make(map[string]int)
		}
		byProto[rec.backend][rec.proto]++
	}
	return total, byProto
}
