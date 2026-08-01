// Package warden implements the yarilo-warden TCP+mTLS connection-accounting server.
// Login pods call it to enforce mail_max_userip_connections across the cluster;
// LMTP backend pods call it to enforce lmtp_user_concurrency_limit.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-warden\t1\t6\n
//	  DONE\n
//
//	Client commands:
//	  CONNECT\t{id}\t{user}\t{ip}\t{service}\n
//	  DISCONNECT\t{id}\t{user}\t{ip}\t{service}\n
//	  WHO[\tservice={s}][\tuser={u}]\n
//	  LOOKUP\t{user}\t{service}\n          ← 1.2 (LMTP cluster-wide accounting)
//	  HEARTBEAT\t{id}\n                    ← 1.3 (renew session TTL)
//	  SELECT\t{id}\t{folder}\n             ← 1.4 (IMAP currently-SELECTed folder)
//	  SUBSCRIBE\t{channel}\n               ← 1.5 (pub/sub for operational events)
//	  EMIT\t{channel}\t{payload}\n         ← 1.5
//	  PENALTY-LOOKUP\t{ip}\n               ← 1.6 (auth-penalty IP backoff)
//	  PENALTY-UPDATE\t{ip}\t{count}\n      ← 1.6
//	  DUMP\n                               ← 1.8 (admin/debug state snapshot)
//
//	Server responses:
//	  OK\t{id}\n
//	  FAIL\t{id}\treason=too-many-connections\n
//	  COUNT\t{n}\n                          ← LOOKUP reply
//	  PENALTY\t{count}\n                    ← PENALTY-LOOKUP reply
//	  BACKEND\t{id}\t{backend_ip}\n        ← 1.7 (backend pod the session routed to)
//	  SESSION\t{id}\t{user}\t{ip}\t{service}\t{connect_unix}\t{folder}\t{backend}\n
//	  EVENT\t{channel}\t{payload}\n         ← server push to subscribers
//	  CNT\t{user@ip}\t{counter}\t{live}\n   ← 1.8 DUMP: counter vs live tally (drift)
//	  PEN\t{ip}\t{count}\t{ttl_secs}\n      ← 1.8 DUMP: penalty entry
//	  DONE\n
//
// WHO replies with one SESSION line per matching session, then DONE.
// LOOKUP replies COUNT with the live (user, service) session count.
// HEARTBEAT renews a session's TTL; a background sweeper drops
// sessions that miss their refresh window.
//
// Unknown commands are ignored.
package warden

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	protoName = "yarilo-warden"
	majorVer  = 1
	minorVer  = 8
)

// DefaultPenaltyDecay is how long a penalty entry survives after its
// last update. 2+4+8+15 = 29s: the worst-case backoff chain length.
const DefaultPenaltyDecay = 29 * time.Second

// MaxPenalty caps the per-IP backoff counter; PenaltyToSecs plateaus there.
const MaxPenalty = 4

// PenaltyToSecs maps a penalty counter to the sleep applied before the
// next auth attempt from that IP. 0 for a clean IP.
func PenaltyToSecs(count int) int {
	switch {
	case count <= 0:
		return 0
	case count == 1:
		return 2
	case count == 2:
		return 4
	case count == 3:
		return 8
	default:
		return 15
	}
}

// DefaultSessionTTL is how long a session lives without a HEARTBEAT.
// 90s budgets three missed 30s heartbeats before drop.
const DefaultSessionTTL = 90 * time.Second

// DefaultSweepInterval is how often stale sessions are reaped.
const DefaultSweepInterval = 15 * time.Second

// SessionInfo is one tracked client connection.
type SessionInfo struct {
	ID          string
	User        string
	IP          string
	Service     string
	ConnectedAt time.Time
	// Folder is the currently-SELECTed IMAP mailbox (empty for non-IMAP).
	Folder string
	// Backend is the backend pod IP the session routed to; empty until
	// the BACKEND command arrives (and for pre-1.7 clients).
	Backend string
	// lastSeen is the last heartbeat (or CONNECT) time; the sweeper owns it.
	lastSeen time.Time
}

// Server is the yarilo-warden TCP server. It wraps a connlimit.Limiter and
// exposes it over the wire protocol so multiple login pods can share state.
type Server struct {
	sessionTTL    time.Duration
	sweepInterval time.Duration
	penaltyDecay  time.Duration

	// state holds sessions, penalties and the kick bus, in memory or
	// Redis; the Redis backend fans kicks out cross-replica via Pub/Sub.
	state StateBackend
}

// ServerOption configures a Server at construction time.
type ServerOption func(*Server)

// WithSessionTTL overrides DefaultSessionTTL.
func WithSessionTTL(d time.Duration) ServerOption {
	return func(s *Server) { s.sessionTTL = d }
}

// WithSweepInterval overrides DefaultSweepInterval.
func WithSweepInterval(d time.Duration) ServerOption {
	return func(s *Server) { s.sweepInterval = d }
}

// WithPenaltyDecay overrides DefaultPenaltyDecay.
func WithPenaltyDecay(d time.Duration) ServerOption {
	return func(s *Server) { s.penaltyDecay = d }
}

// NewServer creates an warden server with the given per-user@IP connection limit.
// max ≤ 0 means unlimited (server still runs but always returns OK).
func NewServer(max int, opts ...ServerOption) *Server {
	s := &Server{
		sessionTTL:    DefaultSessionTTL,
		sweepInterval: DefaultSweepInterval,
		penaltyDecay:  DefaultPenaltyDecay,
	}
	for _, opt := range opts {
		opt(s)
	}
	// build after options so decay/TTL/limit reflect them
	if s.state == nil {
		s.state = newMemoryBackend(s.penaltyDecay, s.sessionTTL, max)
	}
	return s
}

// Sessions returns a snapshot of every tracked session.
func (s *Server) Sessions() []*SessionInfo {
	return s.state.SessionList()
}

// SessionCount returns the number of tracked sessions. For the memory
// backend it takes the hot-path mutex, so it doubles as a liveness probe;
// the Redis backend counts via SCAN and has no local mutex to wedge.
func (s *Server) SessionCount() int {
	return s.state.SessionCount()
}

// Listen binds addr (mTLS when tlsCfg is non-nil) and returns the
// listener, so callers can report readiness only after the bind succeeds.
func (s *Server) Listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		ln, err := tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("warden: listen %s (tls): %w", addr, err)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("warden: listen %s: %w", addr, err)
	}
	return ln, nil
}

// ListenAndServe binds addr and serves it.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := s.Listen(addr, tlsCfg)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves an already-bound listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.sweepLoop(ctx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("warden: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	connections.Inc()
	connectionsTotal.Inc()
	defer connections.Dec()
	rd := bufio.NewReaderSize(conn, 4096)

	fmt.Fprintf(conn, "VERSION\t%s\t%d\t%d\n", protoName, majorVer, minorVer)
	fmt.Fprintf(conn, "DONE\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				_ = err
			}
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 {
			continue
		}
		// only login hot-path verbs report a distinguishing result label
		start := time.Now()
		switch fields[0] {
		case "CONNECT":
			res := s.handleConnect(conn, fields)
			connectTotal.WithLabelValues(res).Inc()
			observeRequest("CONNECT", res, start)
		case "DISCONNECT":
			s.handleDisconnect(conn, fields)
			observeRequest("DISCONNECT", "ok", start)
		case "WHO":
			s.handleWho(conn, fields[1:])
			observeRequest("WHO", "ok", start)
		case "DUMP":
			s.handleDump(conn)
			observeRequest("DUMP", "ok", start)
		case "LOOKUP":
			s.handleLookup(conn, fields)
			observeRequest("LOOKUP", "ok", start)
		case "HEARTBEAT":
			observeRequest("HEARTBEAT", s.handleHeartbeat(conn, fields), start)
		case "SELECT":
			s.handleSelect(conn, fields)
			observeRequest("SELECT", "ok", start)
		case "BACKEND":
			s.handleBackend(conn, fields)
			observeRequest("BACKEND", "ok", start)
		case "EMIT":
			s.handleEmit(conn, fields)
			observeRequest("EMIT", "ok", start)
		case "PENALTY-LOOKUP":
			s.handlePenaltyLookup(conn, fields)
			observeRequest("PENALTY-LOOKUP", "ok", start)
		case "PENALTY-UPDATE":
			s.handlePenaltyUpdate(conn, fields)
			observeRequest("PENALTY-UPDATE", "ok", start)
		case "SUBSCRIBE":
			// takes over the conn for server pushes; blocks until it closes
			s.handleSubscribe(conn, fields)
			return
		}
	}
}

// handleConnect processes: CONNECT\t{id}\t{user}\t{ip}\t{service}
// Returns the metric result label.
func (s *Server) handleConnect(conn net.Conn, fields []string) string {
	if len(fields) < 5 {
		// tolerate the legacy v1.0 4-field form (no service)
		if len(fields) < 4 {
			return "bad_request"
		}
		fields = append(fields, "")
	}
	id, user, ip, service := fields[1], fields[2], fields[3], fields[4]
	ok, err := s.state.SessionConnect(id, user, ip, service)
	if err != nil {
		// fail open: a Redis blip must not block logins; the limit is
		// unenforced until the backend recovers
		slog.Warn("warden: connect state error, failing open", "pod", podID, "sid", id, "user", user, "ip", ip, "err", err)
		fmt.Fprintf(conn, "OK\t%s\n", id)
		return "state_error"
	}
	if !ok {
		slog.Warn("warden: too many connections", "pod", podID, "sid", id, "user", user, "ip", ip, "service", service)
		fmt.Fprintf(conn, "FAIL\t%s\treason=too-many-connections\n", id)
		return "too_many_connections"
	}
	// no cnt= here: SessionLookupCount is a Redis SCAN, too costly on the hot path
	slog.Info("warden: session connect", "pod", podID, "sid", id, "user", user, "ip", ip, "service", service)
	fmt.Fprintf(conn, "OK\t%s\n", id)
	return "ok"
}

// handleDisconnect processes: DISCONNECT\t{id}\t{user}\t{ip}\t{service}
func (s *Server) handleDisconnect(conn net.Conn, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user, ip := fields[1], fields[2], fields[3]
	s.state.SessionDisconnect(id, user, ip)
	slog.Debug("warden: disconnect", "sid", id, "user", user, "ip", ip)
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleWho processes: WHO[\tkey=value]...
// Filter keys: service=, user=. Unknown keys are ignored.
func (s *Server) handleWho(conn net.Conn, args []string) {
	filter := parseFilter(args)
	snap := s.Sessions()
	for _, sess := range snap {
		if v, ok := filter["service"]; ok && !strings.EqualFold(v, sess.Service) {
			continue
		}
		if v, ok := filter["user"]; ok && v != sess.User {
			continue
		}
		fmt.Fprintf(conn, "SESSION\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			sess.ID, sess.User, sess.IP, sess.Service, sess.ConnectedAt.Unix(), sess.Folder, sess.Backend)
	}
	fmt.Fprintln(conn, "DONE")
}

// handleDump processes: DUMP. Replies with counters (with live tally, so
// drift is visible) and penalty entries, then DONE:
//
//	CNT\t{user@ip}\t{counter}\t{live}\n   (repeated)
//	PEN\t{ip}\t{count}\t{ttl_secs}\n      (repeated)
//	DONE\n
//
// A backend error still ends with DONE (best-effort snapshot).
func (s *Server) handleDump(conn net.Conn) {
	d, err := s.state.Dump()
	if err != nil {
		slog.Warn("warden: dump failed", "pod", podID, "err", err)
		fmt.Fprintln(conn, "DONE")
		return
	}
	for _, c := range d.Counters {
		fmt.Fprintf(conn, "CNT\t%s\t%d\t%d\n", c.UserIP, c.Counter, c.Live)
	}
	for _, p := range d.Penalties {
		fmt.Fprintf(conn, "PEN\t%s\t%d\t%d\n", p.IP, p.Count, p.TTLSecs)
	}
	fmt.Fprintln(conn, "DONE")
}

// handleLookup processes: LOOKUP\t{user}\t{service}
// Replies COUNT\t{n}\n with the live (user, service) session count.
// Used by LMTP at RCPT TO to enforce lmtp_user_concurrency_limit.
func (s *Server) handleLookup(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		fmt.Fprintf(conn, "COUNT\t0\n")
		return
	}
	user, service := fields[1], fields[2]
	fmt.Fprintf(conn, "COUNT\t%d\n", s.state.SessionLookupCount(user, service))
}

// handleHeartbeat processes: HEARTBEAT\t{id}\n
// Replies OK\t{id}\n on hit, OK\t{id}\treason=unknown\n on miss so a
// login pod can detect its registration was reaped and reissue CONNECT.
func (s *Server) handleHeartbeat(conn net.Conn, fields []string) string {
	if len(fields) < 2 {
		return "bad_request"
	}
	id := fields[1]
	if !s.state.SessionTouch(id) {
		fmt.Fprintf(conn, "OK\t%s\treason=unknown\n", id)
		return "session_unknown"
	}
	fmt.Fprintf(conn, "OK\t%s\n", id)
	return "ok"
}

// handleSelect processes: SELECT\t{id}\t{folder}\n
// Empty folder clears the field (UNSELECT).
func (s *Server) handleSelect(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	id, folder := fields[1], fields[2]
	if !s.state.SessionSetFolder(id, folder) {
		fmt.Fprintf(conn, "OK\t%s\treason=unknown\n", id)
		return
	}
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleBackend processes: BACKEND\t{id}\t{backend_ip}\n
func (s *Server) handleBackend(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	id, backend := fields[1], fields[2]
	if !s.state.SessionSetBackend(id, backend) {
		fmt.Fprintf(conn, "OK\t%s\treason=unknown\n", id)
		return
	}
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// sweepLoop periodically reaps sessions past their TTL, with the same
// effect as a real DISCONNECT.
func (s *Server) sweepLoop(ctx context.Context) {
	t := time.NewTicker(s.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.state.Maintain(time.Now().UTC())
		}
	}
}

// handlePenaltyLookup processes: PENALTY-LOOKUP\t{ip}
// Replies PENALTY\t{count}\n; 0 when no entry or expired.
func (s *Server) handlePenaltyLookup(conn net.Conn, fields []string) {
	if len(fields) < 2 {
		fmt.Fprintf(conn, "PENALTY\t0\n")
		return
	}
	ip := fields[1]
	count, status := s.state.PenaltyLookup(ip)
	penaltyLookups.WithLabelValues(status).Inc()
	fmt.Fprintf(conn, "PENALTY\t%d\n", count)
}

// handlePenaltyUpdate processes: PENALTY-UPDATE\t{ip}\t{count}
// Replies OK\n. Count is clamped to [0, MaxPenalty]; 0 deletes the entry.
func (s *Server) handlePenaltyUpdate(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		fmt.Fprintf(conn, "OK\n")
		return
	}
	ip := fields[1]
	count, err := strconv.Atoi(fields[2])
	if err != nil {
		fmt.Fprintf(conn, "OK\n")
		return
	}
	if count < 0 {
		count = 0
	}
	if count > MaxPenalty {
		count = MaxPenalty
	}
	s.state.PenaltyUpdate(ip, count)
	if count > 0 {
		penaltyUpdates.WithLabelValues("set").Inc()
		slog.Info("warden: penalty set", "pod", podID, "ip", ip, "count", count)
	} else {
		penaltyUpdates.WithLabelValues("clear").Inc()
		slog.Info("warden: penalty clear", "pod", podID, "ip", ip)
	}
	fmt.Fprintf(conn, "OK\n")
}

// handleEmit processes: EMIT\t{channel}\t{payload}\n
// Best-effort at-most-once publish; OK means "published", not "received".
// Kick correctness never depends on delivery — the director's confirmed
// kill is the backstop — so publish errors are logged and still answered OK.
func (s *Server) handleEmit(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	channel, payload := fields[1], fields[2]
	kickEmitted.Inc()
	if err := s.state.Emit(channel, payload); err != nil {
		slog.Warn("warden: emit failed", "pod", podID, "channel", channel, "err", err)
	} else {
		slog.Info("warden: kick emitted", "pod", podID, "channel", channel, "sess", payload)
	}
	fmt.Fprintf(conn, "OK\n")
}

// handleSubscribe processes: SUBSCRIBE\t{channel}\n
// Replies OK\n once, then pushes EVENT\t{channel}\t{payload}\n lines until
// the conn drops. One conn = one channel. The Redis backend reconnects
// underneath, so only a client disconnect ends the subscription.
func (s *Server) handleSubscribe(conn net.Conn, fields []string) {
	if len(fields) < 2 {
		return
	}
	channel := fields[1]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.state.Subscribe(ctx, channel)
	if err != nil {
		slog.Warn("warden: subscribe failed", "channel", channel, "err", err)
		return
	}

	if _, err := fmt.Fprintf(conn, "OK\n"); err != nil {
		return
	}
	slog.Info("warden: subscribe", "pod", podID, "channel", channel)

	// client-side close surfaces as a Read error and unwinds the writer loop
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()

	for payload := range ch {
		if _, err := fmt.Fprintf(conn, "EVENT\t%s\t%s\n", channel, payload); err != nil {
			return
		}
		kickDelivered.Inc()
		slog.Info("warden: kick delivered", "pod", podID, "channel", channel, "sess", payload)
	}
}

func parseFilter(args []string) map[string]string {
	out := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(k)] = v
	}
	return out
}
