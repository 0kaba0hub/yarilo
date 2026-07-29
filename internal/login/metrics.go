package login

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Phase labels for phaseSeconds. One login walks these in order; each is a
// separate network dependency, which is precisely why the breakdown matters —
// a stalled login is otherwise indistinguishable from a slow client.
const (
	phaseTLSHandshake    = "tls_handshake"
	phasePreamble        = "preamble"
	phaseAuthDial        = "auth_dial"
	phaseAuth            = "auth"
	phaseDirectorLookup  = "director_lookup"
	phaseAnvilConnect    = "anvil_connect"
	phaseBackendDial     = "backend_dial"
	phaseBackendPreamble = "backend_preamble"
)

var (
	// phaseSeconds attributes login latency to one dependency. Buckets run to
	// ~65s deliberately: the failure this metric exists to explain is a 20–27s
	// stall, so a ceiling below that would record only "too slow to measure".
	phaseSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "yarilo",
		Subsystem: "login",
		Name:      "phase_seconds",
		Help:      "Latency of one login phase (tls_handshake, preamble, auth_dial, auth, director_lookup, anvil_connect, backend_dial, backend_preamble).",
		Buckets:   prometheus.ExponentialBuckets(0.001, 2, 16), // 1ms … ~65s
	}, []string{"protocol", "phase"})

	resultTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "login",
		Name:      "result_total",
		Help:      "Login outcomes by protocol and result (ok, auth_failed, unavailable, backend_rejected, preamble_error, tls_error).",
	}, []string{"protocol", "result"})

	sessionsGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "yarilo",
		Subsystem: "login",
		Name:      "sessions",
		Help:      "Currently proxied sessions held open by this login pod.",
	}, []string{"protocol"})
)

func (s *Server) observePhase(phase string, start time.Time) {
	phaseSeconds.WithLabelValues(string(s.opts.Protocol), phase).Observe(time.Since(start).Seconds())
}

func (s *Server) incResult(result string) {
	resultTotal.WithLabelValues(string(s.opts.Protocol), result).Inc()
}
