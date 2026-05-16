// Package monitor implements yarilo-monitor: a sidecar health checker that
// probes backend pods via IMAP/POP3/LMTP login and reports state changes to
// the director (BACKEND-FLUSH on failure, BACKEND-UP on recovery).
//
// Backend list is sourced dynamically from the director ring:
//   - HOST handshake lines seed the initial set on connect
//   - RING-CHANGE pushes add/remove backends as pods scale in/out
//
// Credentials are looked up per-tag; all backends in the same tag share
// the same monitoring account (user/password).
package monitor

import (
	"context"
	"log/slog"
	"sync"
)

// Monitor owns the director client and manages per-backend goroutines.
// Backends are added/removed dynamically as the director ring changes.
type Monitor struct {
	cfg *Config
	dc  *DirectorClient

	mu       sync.Mutex
	monitors map[string]context.CancelFunc // ip → cancel func
}

// New creates a Monitor for the given config.
func New(cfg *Config) *Monitor {
	return &Monitor{
		cfg:      cfg,
		dc:       NewDirectorClient(cfg.DirectorAddr),
		monitors: make(map[string]context.CancelFunc),
	}
}

// Run connects to the director, maintains the backend pool, and blocks until
// ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	go m.dc.Run(ctx) // connect + event loop with auto-reconnect

	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case ev := <-m.dc.Events():
			m.handleEvent(ctx, ev)
		}
	}
}

func (m *Monitor) handleEvent(ctx context.Context, ev BackendEvent) {
	switch ev.Event {
	case "up":
		m.startMonitor(ctx, ev.IP, ev.Port, ev.Tag)
	case "down":
		// Backend permanently removed from ring — stop probing.
		m.stopMonitor(ev.IP)
	case "flush":
		// Backend temporarily unavailable (we or another director marked it down).
		// Keep monitoring so we detect recovery.
	}
}

func (m *Monitor) startMonitor(ctx context.Context, ip string, port int, tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.monitors[ip]; exists {
		return // already monitoring this IP
	}

	bctx, cancel := context.WithCancel(ctx)
	m.monitors[ip] = cancel

	bm := newBackendMonitor(ip, port, tag, m.cfg, m.dc)
	go func() {
		bm.Run(bctx)
		m.mu.Lock()
		delete(m.monitors, ip)
		m.mu.Unlock()
	}()

	slog.Info("monitor: started backend monitor", "ip", ip, "port", port, "tag", tag)
}

func (m *Monitor) stopMonitor(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cancel, ok := m.monitors[ip]; ok {
		cancel()
		delete(m.monitors, ip)
		slog.Info("monitor: stopped backend monitor", "ip", ip)
	}
}

func (m *Monitor) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ip, cancel := range m.monitors {
		cancel()
		delete(m.monitors, ip)
	}
}
