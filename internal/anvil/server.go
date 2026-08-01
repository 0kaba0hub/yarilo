// Package anvil implements the yarilo-anvil TCP+mTLS connection-accounting server.
// Login pods call it to enforce mail_max_userip_connections across the cluster;
// LMTP backend pods call it to enforce lmtp_user_concurrency_limit.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-anvil\t1\t6\n
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
// WHO replies with one SESSION line per matching active session,
// then DONE. The optional service= / user= tokens narrow the list.
// LOOKUP replies with a single COUNT line carrying the live count
// of (user, service) pairs. HEARTBEAT extends a session's TTL by
// SessionTTL — login pods refresh their registrations on a timer
// so a crashed pod doesn't leak entries forever; a background
// sweeper drops sessions that miss their refresh window.
//
// Minor protocol version bumps:
//   - 1.0 → 1.1: WHO command
//   - 1.1 → 1.2: LOOKUP command
//   - 1.2 → 1.3: HEARTBEAT command + TTL/sweeper
//   - 1.3 → 1.4: SELECT command + folder field in SESSION reply
//   - 1.4 → 1.5: SUBSCRIBE / EMIT / EVENT for operational pub-sub
//     (kick events ride this channel)
//   - 1.5 → 1.6: PENALTY-LOOKUP / PENALTY-UPDATE for IP-bound auth
//   - 1.6 → 1.7: BACKEND command + backend field in SESSION reply (#814)
//     backoff (yarilo-auth dials anvil pre-passdb to look up the
//     current penalty for the client IP, sleeps the mapped seconds,
//     then runs the chain; on fail increments the counter, on OK
//     resets it).
//
// Older clients ignore unknown commands entirely.
package anvil

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
	protoName = "yarilo-anvil"
	majorVer  = 1
	minorVer  = 8
)

// DefaultPenaltyDecay is how long a penalty entry survives after
// its last update before the sweeper drops it. Matches the
// AUTH_PENALTY_TIMEOUT formula: 2 + 4 + 8 + 15 = 29s — once an
// IP's worst-case backoff chain has fully played out, the entry
// is stale and can be evicted so a returning attacker starts
// fresh-clean rather than re-amplifying a years-old slot.
const DefaultPenaltyDecay = 29 * time.Second

// MaxPenalty caps the per-IP backoff counter. Above this the
// PenaltyToSecs mapping plateaus at the maximum sleep — extra
// fails simply keep the counter at the cap until decay.
const MaxPenalty = 4

// PenaltyToSecs maps a penalty counter to the sleep duration
// applied BEFORE the next auth attempt from that IP. Cumulative
// budget for 4 successive fails is 2+4+8+15 = 29 seconds; after
// that the cap holds. Returns 0 for counter 0 so the first
// attempt from a clean IP is never slowed.
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

// DefaultSessionTTL is how long an anvil session lives without a
// HEARTBEAT. Login pods refresh on a timer significantly shorter
// than this so a brief network hiccup never reaps a live session.
// 90 seconds = three 30-second heartbeats budgeted before drop.
const DefaultSessionTTL = 90 * time.Second

// DefaultSweepInterval is how often the background goroutine
// walks the session map to drop stale entries. Tighter than TTL
// so a leaked session is gone within `TTL + interval` of the
// pod crash, not `2 * TTL`.
const DefaultSweepInterval = 15 * time.Second

// SessionInfo is one tracked client connection. Exported so the
// client package can return parsed WHO rows directly.
type SessionInfo struct {
	ID          string
	User        string
	IP          string
	Service     string
	ConnectedAt time.Time
	// Folder is the currently-SELECTed IMAP mailbox name (empty
	// for non-IMAP services or sessions that have not yet sent
	// SELECT). Updated via the SELECT wire command and surfaced
	// in WHO output.
	Folder string
	// Backend is the backend pod IP the session was routed to (#814),
	// pushed by the login pod after the director LOOKUP resolves it. Empty
	// until the BACKEND command arrives (and for pre-1.7 clients). Lets `who`
	// show only the sessions on the backend it runs against.
	Backend string
	// lastSeen is the most recent heartbeat (or CONNECT) timestamp.
	// Unexported because callers should not depend on it — the
	// sweeper owns reaping. Wire format never surfaces it.
	lastSeen time.Time
}

// Server is the yarilo-anvil TCP server. It wraps a connlimit.Limiter and
// exposes it over the wire protocol so multiple login pods can share state.
type Server struct {
	sessionTTL    time.Duration
	sweepInterval time.Duration
	penaltyDecay  time.Duration

	// state is the pluggable shared-state backend (#908): sessions (with the
	// per-user@IP connection limit), the penalty counter, and the kick bus, in
	// memory or Redis. Emit/Subscribe delegate to it, so a Redis backend fans
	// kicks out cross-replica via Pub/Sub while memory stays in-process.
	state StateBackend
}

// ServerOption configures a Server at construction time.
type ServerOption func(*Server)

// WithSessionTTL overrides DefaultSessionTTL — how long a
// session lives between heartbeats. Tests use a short TTL so
// the sweeper can be exercised in subsecond time.
func WithSessionTTL(d time.Duration) ServerOption {
	return func(s *Server) { s.sessionTTL = d }
}

// WithSweepInterval overrides DefaultSweepInterval — how often
// the background sweeper drops stale sessions.
func WithSweepInterval(d time.Duration) ServerOption {
	return func(s *Server) { s.sweepInterval = d }
}

// WithPenaltyDecay overrides DefaultPenaltyDecay — how long a
// penalty entry survives after its last update before the
// sweeper drops it.
func WithPenaltyDecay(d time.Duration) ServerOption {
	return func(s *Server) { s.penaltyDecay = d }
}

// NewServer creates an anvil server with the given per-user@IP connection limit.
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
	// Default to the in-process backend unless WithStateBackend injected one; its
	// decay/TTL/limit must reflect any options applied above, so build it here.
	if s.state == nil {
		s.state = newMemoryBackend(s.penaltyDecay, s.sessionTTL, max)
	}
	return s
}

// Sessions returns a snapshot of every tracked session. Used by
// tests and by the in-process accounting path (no wire detour).
func (s *Server) Sessions() []*SessionInfo {
	return s.state.SessionList()
}

// SessionCount returns the number of tracked sessions. For the memory backend it
// takes the same mutex the hot-path handlers hold, so it doubles as the #904
// liveness probe; the Redis backend counts via SCAN, so the anvil watchdog
// self-check is wired only in memory mode (a Redis anvil is like locks — no
// local mutex to wedge, #921).
func (s *Server) SessionCount() int {
	return s.state.SessionCount()
}

// ListenAndServe starts the anvil TCP server. When tlsCfg is non-nil the
// listener uses mTLS. Blocks until ctx is cancelled; active sessions drain
// before the function returns.
// Listen binds addr and returns the listener, so a caller can report readiness
// only once the port is accepting. ListenAndServe binds and serves in one call,
// which forces the caller to run it in a goroutine and therefore to announce
// readiness before knowing whether the bind succeeded.
func (s *Server) Listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		ln, err := tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("anvil: listen %s (tls): %w", addr, err)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("anvil: listen %s: %w", addr, err)
	}
	return ln, nil
}

// ListenAndServe binds addr and serves it. Kept for callers that do not need to
// separate the two.
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
			return fmt.Errorf("anvil: accept: %w", err)
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
		// Every verb is timed at the dispatch point. Only the verbs on the
		// login hot path report a distinguishing result label; the rest are
		// recorded as "ok" because their wire replies carry no uniform outcome
		// worth a separate series.
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
			// SUBSCRIBE takes over the connection for server→client
			// pushes; handleSubscribe blocks until the conn closes
			// or the subscriber's outbox drops. Returning from
			// handleConn after it ends is correct — the for-loop
			// above would just re-read EOF anyway.
			s.handleSubscribe(conn, fields)
			return
		}
	}
}

// handleConnect processes: CONNECT\t{id}\t{user}\t{ip}\t{service}
// The metric result label is returned for the dispatcher to observe.
func (s *Server) handleConnect(conn net.Conn, fields []string) string {
	if len(fields) < 5 {
		// Tolerate the legacy 4-field form (no service) for back-compat
		// with v1.0 clients — service stays empty in the session record.
		if len(fields) < 4 {
			return "bad_request"
		}
		fields = append(fields, "")
	}
	id, user, ip, service := fields[1], fields[2], fields[3], fields[4]
	ok, err := s.state.SessionConnect(id, user, ip, service)
	if err != nil {
		// Bounded backend error → fail OPEN: allow the session (a Redis blip must
		// not block every login), untracked until the backend recovers. The limit
		// is temporarily unenforced — availability over strict limiting, the same
		// posture as the penalty path (#908; verified end to end in PR4).
		slog.Warn("anvil: connect state error, failing open", "pod", podID, "sid", id, "user", user, "ip", ip, "err", err)
		fmt.Fprintf(conn, "OK\t%s\n", id)
		return "state_error"
	}
	if !ok {
		slog.Warn("anvil: too many connections", "pod", podID, "sid", id, "user", user, "ip", ip, "service", service)
		fmt.Fprintf(conn, "FAIL\t%s\treason=too-many-connections\n", id)
		return "too_many_connections"
	}
	// No cnt= here on purpose: SessionLookupCount is a SCAN on the Redis backend,
	// too costly for the hot path (#932 lesson). Use the connect_total /
	// sessions metrics for counts; the log carries pod identity for kick tracing.
	slog.Info("anvil: session connect", "pod", podID, "sid", id, "user", user, "ip", ip, "service", service)
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
	slog.Debug("anvil: disconnect", "sid", id, "user", user, "ip", ip)
	fmt.Fprintf(conn, "OK\t%s\n", id)
}

// handleWho processes: WHO[\tkey=value]...
//
// Recognised filter keys:
//
//	service={imap|pop3|submission|lmtp|...}
//	user={username}
//
// Unknown keys are ignored so future filters can land without
// breaking older anvil servers.
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

// handleDump processes: DUMP (admin/debug introspection, #908 fast-follow).
//
// Replies with the accounting counters (each with its live session tally, so
// drift is visible) and the penalty entries (with remaining TTL), then DONE:
//
//	CNT\t{user@ip}\t{counter}\t{live}\n   (repeated)
//	PEN\t{ip}\t{count}\t{ttl_secs}\n      (repeated)
//	DONE\n
//
// A backend error still ends with DONE (best-effort snapshot); the reader treats
// a short/empty dump as "nothing to show" rather than an error.
func (s *Server) handleDump(conn net.Conn) {
	d, err := s.state.Dump()
	if err != nil {
		slog.Warn("anvil: dump failed", "pod", podID, "err", err)
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
//
// Replies with a single line: COUNT\t{n}\n where n is the live
// number of sessions matching (user, service). Used by LMTP at
// RCPT TO to enforce lmtp_user_concurrency_limit cluster-wide.
//
// Missing service is treated as "any service" — practically this
// means an LMTP delivery counter without a service filter, which
// is over-counting but never under-counting. Real callers always
// supply both.
func (s *Server) handleLookup(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		fmt.Fprintf(conn, "COUNT\t0\n")
		return
	}
	user, service := fields[1], fields[2]
	fmt.Fprintf(conn, "COUNT\t%d\n", s.state.SessionLookupCount(user, service))
}

// handleHeartbeat processes: HEARTBEAT\t{id}\n
//
// Bumps the session's lastSeen so the sweeper does not reap it.
// Replies OK\t{id}\n on hit, OK\t{id}\treason=unknown\n on miss —
// the unknown case lets a login pod detect its registration was
// already reaped and reissue CONNECT. Unknown ID is NOT a hard
// error: the pod is operating on stale information and recovers
// by reconnecting.
// The metric result label is returned for the dispatcher to observe. A
// "session_unknown" outcome is the signal that the sweeper reaped a session
// whose owner still believes it is alive.
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
//
// Sets the session's currently-SELECTed folder. Empty folder is
// the UNSELECT signal — clears the field so WHO renders the
// session as authenticated-but-not-in-a-folder. Unknown id is
// silently ignored: the session may already have been reaped
// (the IMAP client driving the SELECT will notice on its next
// command and recover).
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

// handleBackend records the backend pod IP a session was routed to (#814),
// pushed by the login pod after the director LOOKUP. Mirrors handleSelect.
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

// sweepLoop periodically drops sessions whose lastSeen is older
// than the configured TTL and releases their connection-limit
// slot. Stops when ctx is cancelled (server shutdown).
//
// Reaped sessions are gone from `who`, from LOOKUP counts, and
// their slot is free for the next CONNECT — exactly the
// behaviour a real DISCONNECT would have produced.
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

// sweepStalePenalties drops every penalty entry whose last
// update is older than penaltyDecay. Inner of sweepLoop — takes
// `now` so tests can fast-forward.
// handlePenaltyLookup processes: PENALTY-LOOKUP\t{ip}
// Replies: PENALTY\t{count}\n where count is 0 when no entry
// exists or the entry has expired (lazy eviction on read).
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
// Replies: OK\n. Count is clamped to [0, MaxPenalty]. Count 0
// deletes the entry (matches the auth-success reset path: no
// reason to keep a zero counter around).
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
		slog.Info("anvil: penalty set", "pod", podID, "ip", ip, "count", count)
	} else {
		penaltyUpdates.WithLabelValues("clear").Inc()
		slog.Info("anvil: penalty clear", "pod", podID, "ip", ip)
	}
	fmt.Fprintf(conn, "OK\n")
}

// handleEmit processes: EMIT\t{channel}\t{payload}\n
//
// Delegates to the state backend (in-process fan-out for memory, Redis PUBLISH
// for redis), then replies OK\n. Emit is best-effort / at-most-once, so OK means
// "published", not "received": delivery is NOT the correctness guarantee for a
// kick — the director's #847 confirmed kill holds LOOKUP until the ring-wide
// session count is stably zero, so split-writer safety never depends on the kick
// landing; the kick only makes teardown prompt. A publish error (e.g. a Redis
// blip) is logged and still answered OK — the caller cannot make it more
// reliable, and the confirm loop is the backstop.
func (s *Server) handleEmit(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	channel, payload := fields[1], fields[2]
	kickEmitted.Inc()
	if err := s.state.Emit(channel, payload); err != nil {
		slog.Warn("anvil: emit failed", "pod", podID, "channel", channel, "err", err)
	} else {
		slog.Info("anvil: kick emitted", "pod", podID, "channel", channel, "sess", payload)
	}
	fmt.Fprintf(conn, "OK\n")
}

// handleSubscribe processes: SUBSCRIBE\t{channel}\n
//
// Subscribes via the state backend, replies OK\n once, then pushes
// EVENT\t{channel}\t{payload}\n lines until the conn drops or ctx is cancelled.
// One conn = one channel — callers wanting multiple channels open multiple conns.
//
// A reader goroutine cancels the subscription context when the client conn
// closes, which closes the backend channel and unwinds the writer loop. For the
// Redis backend the backend channel auto-reconnects underneath, so a Redis blip
// does not end this subscription — only a client disconnect does.
func (s *Server) handleSubscribe(conn net.Conn, fields []string) {
	if len(fields) < 2 {
		return
	}
	channel := fields[1]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.state.Subscribe(ctx, channel)
	if err != nil {
		slog.Warn("anvil: subscribe failed", "channel", channel, "err", err)
		return
	}

	if _, err := fmt.Fprintf(conn, "OK\n"); err != nil {
		return
	}
	slog.Info("anvil: subscribe", "pod", podID, "channel", channel)

	// Reader half: a client-side close surfaces as a Read error and cancels ctx,
	// which closes ch and unwinds the writer loop below.
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
		slog.Info("anvil: kick delivered", "pod", podID, "channel", channel, "sess", payload)
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
