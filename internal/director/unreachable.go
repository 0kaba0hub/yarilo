package director

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// Active fast-fail re-route (#782). Between a backend dying and its heartbeat
// lease expiring (#776), the director still hands out the dead pod's IP on
// LOOKUP. When login proxies fail to dial it they report BACKEND-UNREACHABLE;
// once enough DISTINCT proxies corroborate within a window the director evicts
// the backend early (rehash + RING-CHANGE down). The TTL path (#776) stays as
// the backstop for the single-reporter / no-report cases.

func (o *Options) unreachableReporters() int {
	if o.UnreachableReporters <= 0 {
		return 2
	}
	return o.UnreachableReporters
}

func (o *Options) unreachableWindow() time.Duration {
	if o.UnreachableWindow <= 0 {
		return 5 * time.Second
	}
	return o.UnreachableWindow
}

// recordUnreachable registers one reporter's unreachable report for backendIP
// and returns true when the number of DISTINCT reporters still inside the
// window has reached the configured threshold. Stale reporters are pruned on
// every call so the window slides.
func (s *Server) recordUnreachable(backendIP, reporterID string, now time.Time) bool {
	window := s.opts.unreachableWindow()
	threshold := s.opts.unreachableReporters()

	s.unreachMu.Lock()
	defer s.unreachMu.Unlock()

	reporters := s.unreach[backendIP]
	if reporters == nil {
		reporters = make(map[string]time.Time)
		s.unreach[backendIP] = reporters
	}
	reporters[reporterID] = now

	for r, t := range reporters {
		if now.Sub(t) > window {
			delete(reporters, r)
		}
	}
	if len(reporters) == 0 {
		delete(s.unreach, backendIP)
		return false
	}
	return len(reporters) >= threshold
}

// clearUnreachable drops all reports for backendIP — called after an eviction
// and after a fresh BACKEND-UP re-admits the backend, so a revived pod starts
// with a clean slate instead of being re-evicted by stale reports.
func (s *Server) clearUnreachable(backendIP string) {
	s.unreachMu.Lock()
	delete(s.unreach, backendIP)
	s.unreachMu.Unlock()
}

// processUnreachable handles a BACKEND-UNREACHABLE report from a LOGIN CLIENT.
// It replicates the report ring-wide so corroboration aggregates across
// directors (proxies reach different directors via the ClusterIP, so a
// per-director count could never reach the threshold, #804), records it locally,
// and evicts if the threshold is met. The reporter identity rides in the payload
// so a peer counts it under the ORIGINAL reporter, not a fresh second one.
func (s *Server) processUnreachable(backendIP, reporterID string, from *client) {
	s.membership.originate("BACKEND-UNREACHABLE", fmt.Sprintf("%s\t%s", backendIP, proto.TabEscape(reporterID)))
	if s.recordUnreachable(backendIP, reporterID, time.Now()) {
		s.evictUnreachable(backendIP, from)
	}
}

// applyRemoteUnreachable records a ring-gossiped BACKEND-UNREACHABLE report. It
// does NOT re-originate (the envelope path already forwards it) and counts the
// report under the reporter carried in the payload, keeping corroboration
// ring-wide without a gossip copy masquerading as a new reporter.
func (s *Server) applyRemoteUnreachable(payload []string) {
	if len(payload) < 2 {
		return
	}
	backendIP := payload[0]
	reporterID := proto.TabUnescape(payload[1])
	if s.recordUnreachable(backendIP, reporterID, time.Now()) {
		s.evictUnreachable(backendIP, nil)
	}
}

// evictUnreachable removes a corroborated-unreachable backend ahead of its lease
// TTL. Idempotent (a backend already gone is skipped, self-limiting the
// RING-CHANGE gossip when several directors cross at once) and never evicts the
// last backend of a tag (to avoid a total blackhole). from, when non-nil, is the
// login client that triggered the local crossing, excluded from the echo.
func (s *Server) evictUnreachable(ip string, from *client) {
	b := s.ring.GetBackend(ip)
	if b == nil {
		return
	}
	if s.ring.CountBackendsInTag(b.Tag) <= 1 {
		slog.Warn("director: backend reported unreachable but it is the LAST in its tag — keeping to avoid total blackhole",
			"ip", ip, "tag", b.Tag)
		return
	}
	s.recordBackendTomb(ip) // #846: before forgetBackendLease, which clears the seq
	s.forgetBackendLease(ip)
	s.ring.RemoveBackend(ip)
	s.kickSessionsForBackend(ip)
	s.clearUnreachable(ip)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tdown\t%s", ip, b.Tag), from)
	s.updateMetrics()
	slog.Warn("director: backend evicted early via corroborated unreachable reports (fast-fail before TTL)",
		"ip", ip, "tag", b.Tag)
}

// handleBackendUnreachable processes a BACKEND-UNREACHABLE report from a login
// client. The reporter identity is derived from the connection (never trusted
// from the wire), so distinct proxies count as distinct reporters and repeats
// from one proxy count once.
func (s *Server) handleBackendUnreachable(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	s.processUnreachable(ip, clientReporterID(c), c)
	_ = c.WriteLine("OK")
}

// clientReporterID identifies the reporting login proxy by its source IP —
// distinct per pod and stable for the connection. In-cluster ClusterIP traffic
// preserves the source IP, so two proxies reporting the same backend count as two.
func clientReporterID(c *client) string {
	if c == nil || c.conn == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(c.conn.RemoteAddr().String())
	if err != nil {
		return c.conn.RemoteAddr().String()
	}
	return host
}
