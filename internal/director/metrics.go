package director

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// backendInfo carries per-backend state. Value is always 1; status label
	// carries the state: "up" | "flush" | "down".
	backendInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "backend_info",
		Help:      "Backend ring membership and status (1 = present, status label = up|flush|down).",
	}, []string{"ip", "port", "tag", "status"})

	// backendSessions is an approximate count of active users routed to each
	// backend, derived from the userDir TTL window. Exact counts require
	// SESSION-OPEN/SESSION-CLOSE tracking (future work).
	backendSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "director",
		Name:      "backend_sessions",
		Help:      "Approximate number of active user sessions per backend (userDir TTL window).",
	}, []string{"ip", "port", "tag"},
	)
)

// updateMetrics refreshes all backend Prometheus gauges.
// Called after every ring mutation and on every session open/close.
func (s *Server) updateMetrics() {
	backends := s.ring.Backends()

	s.sessionsMu.Lock()
	snap := make(map[string]int, len(s.sessions))
	for ip, n := range s.sessions {
		snap[ip] = n
	}
	s.sessionsMu.Unlock()

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
		backendSessions.WithLabelValues(b.IP, portStr, b.Tag).Set(float64(snap[b.IP]))
	}
}
