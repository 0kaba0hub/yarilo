package director

import (
	"strconv"
	"strings"

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

	// backendSessions is the exact count of active proxied sessions per backend
	// and client-facing protocol. Incremented when biProxy starts, decremented
	// when it returns.
	backendSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "backend_sessions",
		Help:      "Exact number of active proxied sessions per backend and protocol.",
	}, []string{"ip", "port", "tag", "protocol"})

	// joinAccepted / joinRejected count ring DIRECTOR-JOIN outcomes (#750).
	// joinRejected's "reason" label distinguishes cidr_denied (source IP not
	// in join_allowed_nets), no_secret (ring auth not configured),
	// malformed_proof / invalid_proof (HMAC challenge/response failure), and
	// dial_back_failed (#750 phase 2: claimed address didn't answer as a
	// director) — see the accompanying slog.Warn for the specific joiner.
	joinAccepted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "ring_join_accepted_total",
		Help:      "Total number of ring DIRECTOR-JOIN requests accepted.",
	})
	joinRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "ring_join_rejected_total",
		Help:      "Total number of ring DIRECTOR-JOIN requests rejected, labelled by reason.",
	}, []string{"reason"})
)

// updateMetrics refreshes all backend Prometheus gauges.
// Called after every ring mutation.
//
// backend_sessions (below) is always empty now: its only writers,
// Server.sessionOpen/sessionClose, lived in the data-path proxy removed in
// #741 (director is control-plane only — see docs/DEPLOYMENT.md). The gauge
// and the s.sessions map it reads are left in place rather than removed
// unilaterally; whether to delete the metric or wire it to real
// login-pod-reported session counts is a separate decision.
func (s *Server) updateMetrics() {
	backends := s.ring.Backends()

	s.sessionsMu.Lock()
	snap := make(map[string]int, len(s.sessions))
	for k, n := range s.sessions {
		snap[k] = n
	}
	s.sessionsMu.Unlock()

	// Build ip → protocol → count from the snapshot.
	byIPProto := make(map[string]map[string]int)
	for key, n := range snap {
		if n == 0 {
			continue
		}
		parts := strings.SplitN(key, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		ip, proto := parts[0], parts[1]
		if byIPProto[ip] == nil {
			byIPProto[ip] = make(map[string]int)
		}
		byIPProto[ip][proto] += n
	}

	// Clear old series so removed backends / protocols don't linger.
	backendInfo.Reset()
	backendSessions.Reset()

	for _, b := range backends {
		portStr := strconv.Itoa(b.Port)
		status := "up"
		if !b.Up {
			status = "flush"
		}
		backendInfo.WithLabelValues(b.IP, portStr, b.Tag, status).Set(1)

		for proto, cnt := range byIPProto[b.IP] {
			backendSessions.WithLabelValues(b.IP, portStr, b.Tag, proto).Set(float64(cnt))
		}
	}
}
