package buildmail

import "time"

// stageTimes records where one message's indexing time went.
//
// fts_build_seconds measures the whole of it as a single number, which was
// enough to say that indexing is CPU-bound and not enough to say what the CPU
// is doing. The same gap fts_fetch_seconds had when it timed the open and left
// the read inside somebody else's total.
//
// Only leaf operations are timed. Reading a text part is interleaved with
// tokenising it — the reader feeds the tokeniser through a callback — so timing
// it separately would need every layer to subtract its children. Tokenise is
// therefore the remainder: the pass time that is not parsing, decoding or
// writing. Said plainly here because a derived number that looks measured is
// the kind of thing that misleads later.
type stageTimes struct {
	parse  time.Duration
	decode time.Duration
	write  time.Duration
}

// track adds the duration of fn to dst.
func track(dst *time.Duration, fn func() error) error {
	t0 := time.Now()
	err := fn()
	*dst += time.Since(t0)
	return err
}

// observe publishes the split for one message. total is the whole Build call,
// so the remainder attributed to tokenising cannot go negative even if the
// clock is coarse.
func (s stageTimes) observe(total time.Duration) {
	metricBuildStage.WithLabelValues("parse").Observe(s.parse.Seconds())
	metricBuildStage.WithLabelValues("decode").Observe(s.decode.Seconds())
	metricBuildStage.WithLabelValues("write").Observe(s.write.Seconds())

	tokenize := total - s.parse - s.decode - s.write
	if tokenize < 0 {
		tokenize = 0
	}
	metricBuildStage.WithLabelValues("tokenize").Observe(tokenize.Seconds())
}
