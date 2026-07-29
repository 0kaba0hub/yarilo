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

	// transientRetries counts retry attempts actually made, and
	// transientExhausted counts the cases where the budget ran out and the client
	// was told the service is unavailable anyway (#896). The pair is the useful
	// signal: retries alone only say a dependency is flapping, while exhausted
	// says a client saw it. A rising retries count with exhausted flat means the
	// budget is doing its job; both rising means the outage outlasts it.
	transientRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "login",
		Name:      "transient_retries_total",
		Help:      "Retry attempts made after a transient failure, by protocol and stage (auth_dial, auth, backend_session).",
	}, []string{"protocol", "stage"})

	transientExhausted = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "login",
		Name:      "transient_exhausted_total",
		Help:      "Transient failures that exhausted their retry budget and were surfaced to the client, by protocol and stage.",
	}, []string{"protocol", "stage"})

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

// Stage labels for the transient-retry counters.
const (
	stageAuthDial       = "auth_dial"
	stageAuth           = "auth"
	stageBackendSession = "backend_session"
)

func (s *Server) incTransientRetry(stage string) {
	transientRetries.WithLabelValues(string(s.opts.Protocol), stage).Inc()
}

func (s *Server) incTransientExhausted(stage string) {
	transientExhausted.WithLabelValues(string(s.opts.Protocol), stage).Inc()
}
