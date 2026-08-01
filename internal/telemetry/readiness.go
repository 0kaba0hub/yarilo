package telemetry

import (
	"context"
	"crypto/tls"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Check is one named readiness condition; the name appears in the /readyz body
// and as a metric label so a failing probe says which dependency is missing.
type Check struct {
	// Name appears in the /readyz body and as a metric label; keep it short and
	// stable ("auth", "director", "redis", "db").
	Name string
	// Probe reports nil when the dependency is usable. It must honour ctx: /readyz
	// applies a deadline so a hung dependency cannot hang the probe.
	Probe func(ctx context.Context) error
}

// readinessTimeout bounds one /readyz evaluation; kept under kubelet's probe
// timeout so a slow dependency surfaces as "not ready", not as a probe timeout.
const readinessTimeout = 800 * time.Millisecond

// checkGauge publishes each condition so a not-ready pod is diagnosable from metrics.
var checkGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "yarilo",
	Name:      "readiness_check",
	Help:      "Readiness condition result (1 = passing, 0 = failing), by check name.",
}, []string{"check"})

// TCPCheck reports a dependency reachable when a TCP connection to addr succeeds.
// When tlsCfg is set the TLS handshake must also complete, which makes the check
// meaningful under internal mTLS. An empty addr yields a check that always passes.
func TCPCheck(name, addr string, tlsCfg *tls.Config) Check {
	return Check{
		Name: name,
		Probe: func(ctx context.Context) error {
			if addr == "" {
				return nil
			}
			d := &net.Dialer{}
			var conn net.Conn
			var err error
			if tlsCfg != nil {
				conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
			} else {
				conn, err = d.DialContext(ctx, "tcp", addr)
			}
			if err != nil {
				return err
			}
			return conn.Close()
		},
	}
}

// FuncCheck adapts an existing bool predicate into a Check.
func FuncCheck(name string, ok func() bool) Check {
	return Check{
		Name: name,
		Probe: func(context.Context) error {
			if ok == nil || ok() {
				return nil
			}
			return errNotReady{name}
		},
	}
}

type errNotReady struct{ name string }

func (e errNotReady) Error() string { return e.name + ": not ready" }

// checkResult is one evaluated condition.
type checkResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// evaluate runs every check concurrently under one deadline, so readiness latency
// is the slowest dependency rather than their sum.
func evaluate(ctx context.Context, checks []Check) []checkResult {
	if len(checks) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	results := make([]checkResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			res := checkResult{Name: c.Name, OK: true}
			if c.Probe != nil {
				if err := c.Probe(ctx); err != nil {
					res.OK = false
					res.Error = err.Error()
				}
			}
			results[i] = res
		}(i, c)
	}
	wg.Wait()

	for _, r := range results {
		v := 0.0
		if r.OK {
			v = 1
		}
		checkGauge.WithLabelValues(r.Name).Set(v)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}
