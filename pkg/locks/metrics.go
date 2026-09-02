package locks

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the Prometheus metric set emitted by Server. The mode label is
// fixed at construction ("embedded" or "remote") so dashboards can split
// behaviour without explosion-prone per-resource labels.
type Metrics struct {
	acquireSeconds *prometheus.HistogramVec
	busyTotal      prometheus.Counter
	renewFailed    prometheus.Counter

	contenders    *prometheus.GaugeVec
	contendersMu  sync.Mutex
	windowStarted time.Time
	seen          map[string]map[string]struct{} // resource -> owners this window
}

// contenderWindow is how long owners are collected before the gauge is rolled.
const contenderWindow = 30 * time.Second

// observeContender records that owner asked for resource, rolling the window
// when it has run out -- on observation, not on a timer, so a quiet service
// reports its last window rather than a zero. The owner set is what makes this
// a queue depth: one session retrying forty times is one contender (#1640).
func (m *Metrics) observeContender(resource, owner string) {
	if m == nil || m.contenders == nil {
		return
	}
	m.contendersMu.Lock()
	defer m.contendersMu.Unlock()
	if time.Since(m.windowStarted) >= contenderWindow {
		m.rollLocked()
	}
	owners, ok := m.seen[resource]
	if !ok {
		owners = map[string]struct{}{}
		m.seen[resource] = owners
	}
	owners[owner] = struct{}{}
}

// rollLocked publishes, per class, the most owners any single resource had. The
// maximum and not the mean: a queue is a property of one resource.
func (m *Metrics) rollLocked() {
	worst := map[string]int{}
	for resource, owners := range m.seen {
		class := resourceClass(resource)
		if len(owners) > worst[class] {
			worst[class] = len(owners)
		}
	}
	for class, n := range worst {
		m.contenders.WithLabelValues(class).Set(float64(n))
	}
	m.seen = map[string]map[string]struct{}{}
	m.windowStarted = time.Now()
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
		contenders: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "yarilo_locks_resource_contenders",
			Help:        "Distinct owners that asked for one resource of this class in the last window, taking the most-contended resource. A queue depth, not a request count: a session retrying many times counts once.",
			ConstLabels: prometheus.Labels{"mode": mode},
		}, []string{"resource"}),
		seen:          map[string]map[string]struct{}{},
		windowStarted: time.Now(),
	}
	// MustRegister is fine here — duplicate registration in tests is caught
	// loud, and parameters above guarantee non-conflicting metric identity.
	r.MustRegister(m.acquireSeconds, m.busyTotal, m.renewFailed, m.contenders)
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
