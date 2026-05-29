package locks

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the Prometheus metric set emitted by Server. The mode label is
// fixed at construction ("embedded" or "remote") so dashboards can split
// behaviour without explosion-prone per-resource labels.
type Metrics struct {
	acquireSeconds *prometheus.HistogramVec
	busyTotal      prometheus.Counter
	renewFailed    prometheus.Counter
}

// NewMetrics constructs and registers the metric set on r. If r is nil the
// default registry is used. mode is one of "embedded" | "remote" and tags
// every series; pass "" to fall back to "unknown".
func NewMetrics(r prometheus.Registerer, mode string) *Metrics {
	if mode == "" {
		mode = "unknown"
	}
	if r == nil {
		r = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		acquireSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "yarilo_locks_acquire_seconds",
			Help:        "Latency of LOCK acquisition attempts on yarilo-locks (server-side).",
			Buckets:     prometheus.ExponentialBuckets(0.00005, 2, 14), // 50µs … ~400ms
			ConstLabels: prometheus.Labels{"mode": mode},
		}, []string{"result"}), // ok | busy | error
		busyTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "yarilo_locks_busy_total",
			Help:        "Total LOCK requests refused because the resource was held.",
			ConstLabels: prometheus.Labels{"mode": mode},
		}),
		renewFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "yarilo_locks_renew_failed_total",
			Help:        "Total RENEW requests rejected because the lock had already expired.",
			ConstLabels: prometheus.Labels{"mode": mode},
		}),
	}
	// MustRegister is fine here — duplicate registration in tests is caught
	// loud, and parameters above guarantee non-conflicting metric identity.
	r.MustRegister(m.acquireSeconds, m.busyTotal, m.renewFailed)
	return m
}

func (m *Metrics) observeAcquire(seconds float64, result string) {
	if m == nil || m.acquireSeconds == nil {
		return
	}
	m.acquireSeconds.WithLabelValues(result).Observe(seconds)
}

func (m *Metrics) incBusy() {
	if m == nil || m.busyTotal == nil {
		return
	}
	m.busyTotal.Inc()
}

func (m *Metrics) incRenewFailed() {
	if m == nil || m.renewFailed == nil {
		return
	}
	m.renewFailed.Inc()
}
