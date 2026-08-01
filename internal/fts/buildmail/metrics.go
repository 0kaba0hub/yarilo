package buildmail

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricDecoderDegraded counts attachments indexed without extracted text
// after decoder retries were exhausted; the message itself still indexes.
var metricDecoderDegraded = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fts_decoder_degraded_total",
	Help: "Attachments indexed without extracted text because decoder retries were exhausted.",
})
