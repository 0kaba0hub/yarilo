package scheme

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// verifySeconds separates password-scheme cost from everything else on the auth
// path. BCRYPT and SHA512-CRYPT are deliberately expensive by design, so a
// login slowdown caused by a raised cost factor is indistinguishable from a slow
// passdb query without this split.
//
// The metric is emitted by whichever process performs the verification — that is
// yarilo-auth for wire logins, and the session binaries for in-process auth.
var verifySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "yarilo",
	Subsystem: "auth",
	Name:      "scheme_verify_seconds",
	Help:      "Latency of a single password-scheme verification, by scheme.",
	Buckets:   prometheus.ExponentialBuckets(0.0005, 2, 14), // 0.5ms … ~4s
}, []string{"scheme"})

func observeVerify(scheme string, start time.Time) {
	if scheme == "" {
		scheme = "unknown"
	}
	verifySeconds.WithLabelValues(scheme).Observe(time.Since(start).Seconds())
}
