package buildmail

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricBuildStage splits an index pass by what it was doing.
//
// fts_build_seconds says indexing is CPU-bound; this says which CPU work.
// Decoding an attachment that yields no useful terms and tokenising a message
// body cost the same in the total and call for opposite fixes — one is a
// config question about what is worth indexing, the other is a question about
// the tokeniser.
//
// Four label values, so the cardinality is fixed: parse, decode, tokenize,
// write.
var metricBuildStage = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fts_build_stage_seconds",
	Help:    "Time one message spent in each stage of indexing.",
	Buckets: []float64{0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
}, []string{"stage"})

// metricDecoderDegraded counts attachments indexed without extracted text
// after decoder retries were exhausted; the message itself still indexes.
var metricDecoderDegraded = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fts_decoder_degraded_total",
	Help: "Attachments indexed without extracted text because decoder retries were exhausted.",
})
