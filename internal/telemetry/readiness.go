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

// Check is one readiness condition, named so a failing /readyz says which
// dependency is missing instead of only that the pod is not ready.
//
// The point of this type is that wiring a real condition is one line at the call
// site — see TCPCheck. Before this existed, eleven of fourteen components
// answered /readyz with an unconditional 200, so kubelet routed traffic to pods
// that could not reach the dependencies they needed.
type Check struct {
	// Name appears in the /readyz body and as a metric label. Keep it short and
	// stable: "auth", "director", "redis", "db".
	Name string
	// Probe reports nil when the dependency is usable. It must honour ctx —
	// /readyz applies a deadline so a hung dependency cannot hang the probe.
	Probe func(ctx context.Context) error
}

// readinessTimeout bounds one /readyz evaluation. Kubelet's own probe timeout is
// typically a second or more; staying under it means a slow dependency surfaces
// as "not ready" rather than as a probe timeout, which reads the same to
// Kubernetes but tells an operator far less.
const readinessTimeout = 800 * time.Millisecond

// checkGauge publishes each condition so a not-ready pod can be diagnosed from
// metrics without shelling into it.
var checkGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "yarilo",
	Name:      "readiness_check",
	Help:      "Readiness condition result (1 = passing, 0 = failing), by check name.",
}, []string{"check"})

// TCPCheck reports a dependency reachable when a TCP connection to addr can be
// established. tlsCfg is optional: when set, the TLS handshake must also
// complete, which is what makes the check meaningful under internal mTLS — a
// component whose certificate is wrong is not usable even though the port
// accepts.
//
// An empty addr yields a check that always passes, so a caller can wire a
// dependency unconditionally and let configuration decide whether it applies.
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

// FuncCheck adapts an existing predicate — a backend's own "am I connected"
// accessor, for instance — into a Check.
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

// evaluate runs every check concurrently under one deadline. Concurrent because
// readiness latency is then the slowest dependency rather than their sum, which
// matters once a component has several.
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
