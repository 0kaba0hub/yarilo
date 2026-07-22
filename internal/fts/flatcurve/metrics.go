//go:build flatcurve

package flatcurve

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Optimize Prometheus metrics (#715). Registered on the default registry,
// which the telemetry /metrics handler serves.
var (
	metricOptimizeRuns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_optimize_runs_total",
		Help: "Completed flatcurve shard-optimize runs (manual or automatic).",
	})
	metricOptimizeShardsMerged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_optimize_shards_merged_total",
		Help: "Shards merged across all flatcurve optimize runs.",
	})
)
