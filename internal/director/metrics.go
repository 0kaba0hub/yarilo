package director

import (
	"net"
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

// updateMetrics refreshes all backend Prometheus gauges from the current ring
// and userDir state. Called after every ring mutation and on a periodic tick.
func (s *Server) updateMetrics() {
	backends := s.ring.Backends()
	sessions := s.userDir.CountByBackend()

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

		addr := net.JoinHostPort(b.IP, portStr)
		cnt := float64(sessions[addr])
		backendSessions.WithLabelValues(b.IP, portStr, b.Tag).Set(cnt)
	}
}
