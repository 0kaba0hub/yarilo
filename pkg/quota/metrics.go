package quota

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Counting usage opens every folder, and each open takes the folder lock the
// driver and the index already contend on (#1630).
var (
	MetricFoldersOpened = promauto.NewCounter(prometheus.CounterOpts{
		Name: "quota_folders_opened_total",
		Help: "Folders opened to count a user's usage. Each open takes the folder lock, so this is quota's share of fileindex_lock_acquired_total{site=\"open-probe\"}.",
	})
	MetricUsageCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "quota_usage_count_total",
		Help: "Usage counts asked for, by whether the cached answer was still good. A miss walks every folder.",
	}, []string{"result"}) // hit | miss
)
