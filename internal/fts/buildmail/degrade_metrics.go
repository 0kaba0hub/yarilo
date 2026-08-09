package buildmail

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricDegraded counts messages indexed with less than their full structure
// understood. Not an error: the mail is indexed and searchable by whatever was
// readable. It is counted because the alternative -- a silent partial index --
// is indistinguishable from a complete one, and because the total is the
// number that says how much of an account search can actually see.
var metricDegraded = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fts_index_degraded_total",
	Help: "Messages indexed with part of their structure unreadable, by what could not be read.",
}, []string{"reason"})
