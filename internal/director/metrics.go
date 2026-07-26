package director

import (
	"strconv"

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
)

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
