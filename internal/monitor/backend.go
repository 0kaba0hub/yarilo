package monitor

import (
	"context"
	"log/slog"
	"time"
)

// BackendMonitor polls one backend pod in a loop and reports state changes
// to the director via dc. One goroutine per backend IP.
type BackendMonitor struct {
	ip   string
	port int // port from the ring (used for BACKEND-UP reports)
	tag  string
	cfg  *Config
	dc   *DirectorClient

	consecutiveFails int
	isDown           bool
}

func newBackendMonitor(ip string, port int, tag string, cfg *Config, dc *DirectorClient) *BackendMonitor {
	return &BackendMonitor{ip: ip, port: port, tag: tag, cfg: cfg, dc: dc}
}

// Run polls until ctx is cancelled.
func (b *BackendMonitor) Run(ctx context.Context) {
	log := slog.With("ip", b.ip, "tag", b.tag)
	log.Info("monitor: starting backend poll")

	b.poll(log) // immediate first check
	ticker := time.NewTicker(b.cfg.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.poll(log)
		}
	}
}

func (b *BackendMonitor) poll(log *slog.Logger) {
	user, pass := b.cfg.credentials(b.tag)
	timeout := b.cfg.timeout()
	ok := b.probeAll(user, pass, timeout)

	if ok {
		if b.isDown {
			log.Info("monitor: backend recovered, reporting UP")
			b.dc.ReportUp(b.ip, b.port, b.tag)
			b.isDown = false
		}
		b.consecutiveFails = 0
		return
	}

	b.consecutiveFails++
	log.Warn("monitor: probe failed", "consecutiveFails", b.consecutiveFails)

	if b.consecutiveFails < b.cfg.retryCount() {
		return
	}

	// Enough consecutive failures — confirm with rapid poll.
	if b.cfg.rapidRounds() == 0 || !b.rapidPoll(user, pass, timeout) {
		if !b.isDown {
			log.Warn("monitor: backend declared down, reporting FLUSH")
			b.dc.ReportFlush(b.ip)
			b.isDown = true
		}
	}
	b.consecutiveFails = 0
}

// probeAll runs every enabled protocol probe. Returns false if any fails.
func (b *BackendMonitor) probeAll(user, pass string, timeout time.Duration) bool {
	if b.cfg.PollIMAP {
		if probeIMAP(b.ip, b.cfg.imapPort(), user, pass, timeout) != probeOK {
			return false
		}
	}
	if b.cfg.PollPOP3 {
		if probePOP3(b.ip, b.cfg.pop3Port(), user, pass, timeout) != probeOK {
			return false
		}
	}
	if b.cfg.PollLMTP {
		if probeLMTP(b.ip, b.cfg.lmtpPort(), timeout) != probeOK {
			return false
		}
	}
	return true
}

// rapidPoll runs multiple quick probe rounds to confirm a failure.
// Returns true if the backend is still reachable (failed < threshold).
func (b *BackendMonitor) rapidPoll(user, pass string, timeout time.Duration) bool {
	rounds := b.cfg.rapidRounds()
	needed := b.cfg.rapidFailsNeeded()
	failed := 0

	for i := 0; i < rounds; i++ {
		if !b.probeAll(user, pass, timeout) {
			failed++
		}
	}

	slog.Info("monitor: rapid poll done", "ip", b.ip, "failed", failed, "rounds", rounds)
	return failed <= needed
}
