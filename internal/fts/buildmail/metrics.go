package buildmail

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricDecoderDegraded counts attachments indexed WITHOUT extracted text
// because the decoder's bounded retries were exhausted against a transient
// condition (#697) — the message still indexes successfully otherwise, just
// missing this one attachment's text, and does not get a second autoindex
// pass for it.
var metricDecoderDegraded = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fts_decoder_degraded_total",
	Help: "Attachments indexed without extracted text because decoder retries were exhausted.",
})
