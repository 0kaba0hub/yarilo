// Package director implements the yarilo-director TCP+mTLS routing server.
// Login pods call it to resolve which backend pod should handle a given user,
// and health pods call it to register/deregister backends from the hash ring.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  HOST-HAND-START\n
//	  HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{hostname}\n  (one per backend)
//	  HOST-HAND-END\n
//	  DONE\n
//
//	Client → Server handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  ME\t{ip}\t{port}\t{ts}\n
//	  DONE\n
//
//	Ring membership handshake (#750 — self-organizing ring, replaces the
//	static full-mesh peer list; see membership.go and INTERNALS.md §1):
//	  DIRECTOR-JOIN\t{ip}\t{port}\n        (sent instead of ME/DONE, on a fresh connection to a seed)
//	  JOIN-CHALLENGE\t{nonce_hex}\n
//	  JOIN-PROOF\t{hmac_hex}\n             (HMAC-SHA256(ring_secret, nonce+ip+port))
//	  JOIN-OK\n / JOIN-FAIL\t{reason}\n
//	  DIRECTOR-LIST\t{ip1}:{port1},...\n   (existing members; joiner adds itself and dials its right neighbor separately)
//	  CONNECT\t{ip}\t{port}\n              (sent on a ring/PEER connection: wrong target, dial here instead)
//	  DIRECTOR-ADD\t{originIP}\t{originPort}\t{seq}\t{ip}\t{port}\n
//	  DIRECTOR-REMOVE\t{originIP}\t{originPort}\t{seq}\t{ip}\t{port}\n
//
//	Client commands:
//	  LOOKUP\t{id}\t{user}\t{tag}\n                  (tag required; "" = untagged pool, not "any tag")
//	  SESSION-OPEN\t{id}\t{user}\t{backendIP}\n
//	  SESSION-CLOSE\t{id}\n
//	  BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}\n   (vhosts optional; 0 = 100)
//	  BACKEND-DOWN\t{ip}\n
//	  BACKEND-FLUSH\t{ip}\n                          (drain: stop new lookups, keep backend in registry)
//	  HOST-REMOVE\t{ip}\n                            (alias for BACKEND-DOWN)
//	  USER-MOVE\t{user}\t{ip}\t{port}\n              (force user to specific backend)
//	  USER-RELEASE\t{user}\n                         (remove user override)
//	  USER-WEAK\t{user}\n                            (mark current assignment as soft/weak)
//	  USER-KICK\t{user}\n                            (broadcast kick to all login clients)
//	  USER-KILLED\t{hash}\n                          (login reports sessions closed for hash)
//	  PING\n
//	  QUIT\t{reason}\n
//
//	Server responses:
//	  HOST\t{id}\t{ip}\t{port}\t{tag}\n
//	  FAIL\t{id}\treason=no-backends\n
//	  OK\n
//	  PONG\n
//
//	Server pushes (unsolicited, to local login clients — plain form, unchanged):
//	  RING-CHANGE\t{ip}\t{event}\t{tag}\n            (event: up | down | flush)
//	  USER-MOVED\t{user}\t{ip}\t{port}\n             (when user is moved by another client)
//	  USER-KICKED\t{user}\n                          (broadcast kick notification)
//	  USER-KILLED-EVERYWHERE\t{hash}\n               (director confirms all sessions gone)
//
//	Server pushes (ring-envelope form, right-neighbor connections only, #750):
//	  RING-CHANGE\t{originIP}\t{originPort}\t{seq}\t{ip}\t{event}\t{tag}\n
//	  USER-MOVED\t{originIP}\t{originPort}\t{seq}\t{user}\t{ip}\t{port}\n
//	  USER-KICKED\t{originIP}\t{originPort}\t{seq}\t{user}\n
package director

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

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

const (
	protoName = "yarilo-director"
	majorVer  = 1
	minorVer  = 0
)

// Options configures Server behaviour.
type Options struct {
	UserExpire   time.Duration // how long a user→backend mapping lives; default 900s
	PingInterval time.Duration // idle time before sending PING; default 30s
	PingTimeout  time.Duration // time to wait for PONG before closing; default 10s
	// PeerTLS, LocalIP, LocalPort identify and secure this node's ring
	// connections (dial-out to its right neighbor, and the JOIN dial to a
	// seed) — see StartMembership.
	PeerTLS   *tls.Config
	LocalIP   string
	LocalPort int
	// RingSecret authenticates incoming DIRECTOR-JOIN requests via HMAC-SHA256
	// (#750). Empty means ring auth is disabled — every JOIN is rejected, so
	// this node can only ever run as a singleton (N=1) ring.
	RingSecret []byte
	// MinMembers is an install-time warning threshold only ("below this =
	// no state redundancy") — it never refuses service at any member count.
	MinMembers int
	// AntiEntropyInterval is how often each member re-broadcasts its
	// member+tombstone snapshot over every live ring connection (#759) —
	// a bounded safety net that heals any membership split where at least
	// one connection crosses the two views, without waiting for an event
	// to be (possibly unluckily) dropped and never re-sent. 0 = default
	// (3s); negative = disabled (unit tests that assert on exact
	// propagation paths).
	AntiEntropyInterval time.Duration
	// SeedPollInterval is how often joinLoop re-polls a seed AFTER the
	// initial join succeeded (#759 Fix A). The seed (a shared ClusterIP
	// every pod can always reach) is the one guaranteed crossing point
	// between arbitrarily-partitioned member views — connection-bound
	// mechanisms (broadcast, anti-entropy) cannot heal a split with no
	// crossing connection, so this bounds every partition's lifetime by
	// the poll interval. Full cadence while the view holds fewer than
	// MinMembers, easing to SeedPollIdleInterval once the target size is
	// reached — see joinLoop for why the gate is the target size and
	// never own-view stability. 0 = default (2s); negative = legacy
	// one-shot join (join once, never poll again).
	SeedPollInterval time.Duration
	// SeedPollIdleInterval is the eased poll cadence once the view has
	// reached MinMembers. Defaults to the SAME 2s as SeedPollInterval —
	// i.e. no effective backoff — because a node cannot tell "converged"
	// from "stable but holding a dead member", so a lazy idle cadence
	// leaves a freshly-respawned pod's stale member in place for a full
	// interval (#765). Raise it only to trade steady-state polling for
	// slower dead-member eviction on fresh joiners. Clamped up to
	// SeedPollInterval; 0 = default (2s).
	SeedPollIdleInterval time.Duration
	// TombstoneTTL bounds how long a dead member's tombstone is kept and
	// gossiped (#765) — long enough to outlive propagation delay, short
	// enough that churn across many rollouts can't grow the set forever.
	// Safe to expire because neighbor liveness monitoring (#768) plus the
	// anti-entropy view convergence re-evict a resurrected-but-unreachable
	// member within seconds regardless. 0 = default (10m); negative =
	// never expire.
	TombstoneTTL time.Duration
	// UsernameHashLowercase lowercases usernames before hashing/keying them
	// (director_username_hash_lowercase, #738) — matches the reference
	// implementation's default hash template so two spellings of the same
	// account route to the same backend. Defaults to true.
	UsernameHashLowercase *bool
}

func (o *Options) usernameHashLowercase() bool {
	if o.UsernameHashLowercase == nil {
		return true
	}
	return *o.UsernameHashLowercase
}

func (o *Options) userExpire() time.Duration {
	if o.UserExpire <= 0 {
		return 900 * time.Second
	}
	return o.UserExpire
}

func (o *Options) pingInterval() time.Duration {
	if o.PingInterval <= 0 {
		return 30 * time.Second
	}
	return o.PingInterval
}

func (o *Options) pingTimeout() time.Duration {
	if o.PingTimeout <= 0 {
		return 10 * time.Second
	}
	return o.PingTimeout
}

// client wraps an active connection with a per-connection write lock so
// unsolicited pushes never interleave with command responses.
type client struct {
	conn   net.Conn
	mu     sync.Mutex
	pongCh chan struct{} // receives a token each time PONG is received
	// isPeer marks a connection as another director replica's PeerDialer
	// (identified by the "PEER" handshake line, #700) rather than a login
	// proxy — broadcastToLogins uses this to stop a peer-originated event
	// from being relayed back out to peer connections, which is what
	// caused USER-KICKED to ping-pong forever between replicas in a
	// full-mesh topology.
	isPeer bool
}

func (c *client) WriteLine(line string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.conn, line+"\n")
	return err
}

// sessionRec tracks one active proxied session reported by a login-pod.
type sessionRec struct {
	id      string
	user    string
	backend string  // backend IP (without port)
	cl      *client // which login-pod connection owns this session
}

// Server is the yarilo-director TCP server.
type Server struct {
	ring *ring.Ring
	opts Options

	// userDir stores user→backend mappings with TTL.
	userDir *UserDir

	// overrides maps username → "ip:port" for admin-forced assignments.
	overrideMu sync.RWMutex
	overrides  map[string]string

	// clients is the registry of all currently connected clients.
	clientMu sync.RWMutex
	clients  map[*client]struct{}

	// lmtp session counter per backend IP (used for metrics only).
	sessionsMu sync.Mutex
	sessions   map[string]int

	// sessRec tracks active proxied sessions reported via SESSION-OPEN/CLOSE.
	// Director uses this to send USER-KICKED when a backend goes down.
	sessRecMu sync.RWMutex
	sessById  map[string]*sessionRec     // sessionID → record
	sessByBE  map[string]map[string]bool // backendIP → set of sessionIDs

	// membership owns the self-organizing ring: member list, the single
	// right-neighbor connection, and (origin, seq) event forwarding (#750).
	membership *Membership
}

// New creates a director server with an empty ring and default options.
func New() *Server {
	return NewWithOptions(Options{})
}

// NewWithOptions creates a director server with custom options.
func NewWithOptions(opts Options) *Server {
	s := &Server{
		ring:      ring.New(opts.usernameHashLowercase()),
		opts:      opts,
		userDir:   NewUserDir(opts.userExpire(), opts.usernameHashLowercase(), Member{IP: opts.LocalIP, Port: opts.LocalPort}.String()),
		overrides: make(map[string]string),
		clients:   make(map[*client]struct{}),
		sessions:  make(map[string]int),
		sessById:  make(map[string]*sessionRec),
		sessByBE:  make(map[string]map[string]bool),
	}
	s.membership = NewMembership(s, Member{IP: opts.LocalIP, Port: opts.LocalPort}, opts.RingSecret, opts.PeerTLS, opts.MinMembers)
	s.membership.antiEntropyInterval = opts.AntiEntropyInterval
	s.membership.seedPollInterval = opts.SeedPollInterval
	s.membership.seedPollIdleInterval = opts.SeedPollIdleInterval
	s.membership.tombstoneTTL = opts.TombstoneTTL
	return s
}

// normalizeUser applies the #738 username hash-normalization at the single
// ingress point for every wire/HTTP handler that turns a raw username into a
// hash or map key (handleLookup, handleUserMove, handleUserWeak,
// handleUserRelease, handleUserKick, RouteUser, apiUserMove, apiUserKick).
// Everything downstream of these call sites — s.overrides, s.userDir,
// s.ring — already operates on normalized usernames, so no other call site
// needs its own normalize call.
//
// Ordering with #701 (LOOKUP field TAB-escaping): once TabUnescape lands,
// normalize must run AFTER unescape — normalizeUser(TabUnescape(raw)), never
// the other way round — since escaping is reversible byte-for-byte and
// lowercasing is not.
func (s *Server) normalizeUser(username string) string {
	if !s.opts.usernameHashLowercase() {
		return username
	}
	return ring.NormalizeUsername(username)
}

// StartMembership begins the self-organizing ring (#750): if seeds is
// non-empty, this node tries to join via each in turn; either way it starts
// (or already is) a valid N=1 ring immediately and never refuses service
// while a join is pending.
func (s *Server) StartMembership(ctx context.Context, seeds []string) {
	s.membership.Start(ctx, seeds)
}

// ListPeers returns the current ring membership (self included), formatted
// as "ip:port" strings — kept for API/CLI compatibility (yarilo-admin
// `director ring status`); semantics changed from "statically configured
// full-mesh peers" to "current self-organized ring members" (#750).
func (s *Server) ListPeers() []string {
	members := s.membership.Members()
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.String()
	}
	return out
}

// ListenAndServe starts the director TCP server. When tlsCfg is non-nil the
// listener uses mTLS. Blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	var ln net.Listener
	var err error
	if tlsCfg != nil {
		ln, err = tls.Listen("tcp", addr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("director: listen %s: %w", addr, err)
	}
	go s.purgeLoop(ctx)
	return s.listenOn(ctx, ln)
}

// purgeLoop periodically removes expired userDir entries.
func (s *Server) purgeLoop(ctx context.Context) {
	interval := s.opts.userExpire()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.userDir.Purge()
		}
	}
}

func (s *Server) listenOn(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("director: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) addClient(c *client) {
	s.clientMu.Lock()
	s.clients[c] = struct{}{}
	s.clientMu.Unlock()
}

func (s *Server) removeClient(c *client) {
	s.clientMu.Lock()
	delete(s.clients, c)
	s.clientMu.Unlock()
	s.removeClientSessions(c)
}

// broadcast sends an unsolicited line to all connected clients except the one
// that triggered the change.
func (s *Server) broadcast(line string, exclude *client) {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	for c := range s.clients {
		if c == exclude {
			continue
		}
		_ = c.WriteLine(line)
	}
}

// broadcastToLogins sends an unsolicited line to login-proxy clients only,
// never to peer connections (#700). Used to re-broadcast a peer-originated
// event locally without relaying it back out to other director replicas —
// the origin director's own broadcast already reached every peer directly
// (full-mesh), so a peer relaying it further is exactly the ping-pong loop
// this method exists to avoid.
func (s *Server) broadcastToLogins(line string) {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	for c := range s.clients {
		if c.isPeer {
			continue
		}
		_ = c.WriteLine(line)
	}
}

// originateRingEvent delivers a RING-CHANGE/USER-MOVED/USER-KICKED event
// this node just authored: local login clients get the plain historical
// line (exclude, when non-nil, skips the client that triggered it — e.g.
// the health pod whose own BACKEND-UP doesn't need echoing back), and the
// ring (#750) gets the (origin, seq)-enveloped form via Membership.originate
// for propagation to the right neighbor and, eventually, every member.
func (s *Server) originateRingEvent(kind, payload string, exclude *client) {
	s.clientMu.RLock()
	for c := range s.clients {
		if c == exclude || c.isPeer {
			continue
		}
		_ = c.WriteLine(kind + "\t" + payload)
	}
	s.clientMu.RUnlock()
	s.membership.originate(kind, payload)
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	c := &client{conn: conn, pongCh: make(chan struct{}, 4)}
	s.addClient(c)
	defer s.removeClient(c)

	rd := bufio.NewReaderSize(conn, 4096)

	// Send server handshake: VERSION + current ring state + DONE.
	_ = c.WriteLine(fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer))
	_ = c.WriteLine("HOST-HAND-START")
	for _, b := range s.ring.Backends() {
		_ = c.WriteLine(hostLine(b))
	}
	_ = c.WriteLine("HOST-HAND-END")
	_ = c.WriteLine("DONE")

	// Read client handshake — consume until DONE.
	var dialer Member
	for {
		line, err := readBoundedLine(rd)
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		if line == "DONE" {
			break
		}
		fields := strings.Split(line, "\t")
		switch {
		case fields[0] == "DIRECTOR-JOIN":
			// Sent instead of ME/DONE, on a fresh connection to a seed
			// (#750) — handleJoin owns the rest of this connection's
			// lifetime (challenge/response, DIRECTOR-LIST, close).
			s.membership.handleJoin(conn, fields)
			return
		case len(fields) >= 3 && fields[0] == "ME":
			slog.Debug("director: client identified", "ip", fields[1], "port", fields[2])
			if port, pErr := strconv.Atoi(fields[2]); pErr == nil {
				dialer = Member{IP: fields[1], Port: port}
			}
		case fields[0] == "MEMBERS" && len(fields) >= 3:
			// Sent by a ring dialer right before PEER (#754) — merged
			// BEFORE the PEER case's CONNECT-redirect check below, so that
			// decision uses the dialer's membership view too, not just
			// this node's own (possibly stale) one.
			s.membership.mergeMembers(parseMemberList(fields[1]), parseMemberList(fields[2]))
		case fields[0] == "PEER":
			// Sent only by another director replica's ring connection
			// (#700, repurposed for the ring's right-neighbor dial in
			// #750) — a login proxy's generic cluster/proto dialer never
			// sends this. Distinguishes a ring connection from a login
			// client so broadcastToLogins can stop a peer-originated
			// event from being relayed back out to other ring
			// connections, and so CONNECT-redirect only ever applies here.
			c.isPeer = true
			if !dialer.isZero() {
				if want, redirect := s.membership.checkRedirect(dialer); redirect {
					_ = c.WriteLine(fmt.Sprintf("CONNECT\t%s\t%d", want.IP, want.Port))
					return
				}
			}
		}
	}

	if c.isPeer {
		// Ring connections have their own dedicated line-processing loop
		// (#750) — reuses rd rather than a fresh bufio.Reader so no
		// already-buffered bytes from the handshake read are lost.
		s.membership.serveRingConn(conn, rd, dialer)
		return
	}

	// Start PING/PONG keepalive goroutine.
	stopPing := make(chan struct{})
	go s.pingLoop(c, stopPing)
	defer close(stopPing)

	for {
		line, err := readBoundedLine(rd)
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
		switch fields[0] {
		case "LOOKUP":
			s.handleLookup(c, fields)
		case "SESSION-OPEN":
			s.handleSessionOpen(c, fields)
		case "SESSION-CLOSE":
			s.handleSessionClose(c, fields)
		case "BACKEND-UP":
			s.handleBackendUp(c, fields)
		case "BACKEND-DOWN", "HOST-REMOVE":
			s.handleBackendDown(c, fields)
		case "BACKEND-FLUSH":
			s.handleBackendFlush(c, fields)
		case "USER-MOVE":
			s.handleUserMove(c, fields)
		case "USER-RELEASE":
			s.handleUserRelease(c, fields)
		case "USER-WEAK":
			s.handleUserWeak(c, fields)
		case "USER-KICK":
			s.handleUserKick(c, fields)
		case "USER-KILLED":
			s.handleUserKilled(c, fields)
		case "PONG":
			select {
			case c.pongCh <- struct{}{}:
			default:
			}
		case "PING":
			_ = c.WriteLine("PONG")
		case "QUIT":
			reason := ""
			if len(fields) >= 2 {
				reason = fields[1]
			}
			slog.Info("director: client quit", "reason", reason)
			return
		}
	}
}

// pingLoop sends periodic PING probes and closes the connection if PONG is not received.
func (s *Server) pingLoop(c *client, stop <-chan struct{}) {
	interval := s.opts.pingInterval()
	timeout := s.opts.pingTimeout()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := c.WriteLine("PING"); err != nil {
				return
			}
			// Wait for PONG within timeout.
			timer := time.NewTimer(timeout)
			select {
			case <-c.pongCh:
				timer.Stop()
			case <-timer.C:
				slog.Warn("director: PONG timeout, closing connection")
				c.conn.Close()
				return
			case <-stop:
				timer.Stop()
				return
			}
		}
	}
}

// hostLine formats a Backend as a HOST wire line for the handshake.
func hostLine(b ring.Backend) string {
	return fmt.Sprintf("HOST\t%s\t%d\t%s\tD%d\tU%d\t%s",
		b.IP, b.Port, b.Tag, b.LastDown, b.LastUp, b.Hostname)
}

// handleLookup processes: LOOKUP\t{id}\t{user}\t{tag}
// tag is required; "" restricts to the untagged pool — there is no
// full-ring mode (#737: a login pod belongs to exactly one tag-pool, per
// DEPLOYMENT.md's tag-based sharding model).
// Checks admin overrides first, then ring lookup restricted to tag.
// Response: HOST\t{id}\t{ip}\t{port}\t{tag}
func (s *Server) handleLookup(c *client, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user := fields[1], s.normalizeUser(fields[2])
	tag := fields[3]

	// Admin override wins everything.
	s.overrideMu.RLock()
	addr, hasOverride := s.overrides[user]
	s.overrideMu.RUnlock()

	if hasOverride {
		host, portStr, err := net.SplitHostPort(addr)
		if err == nil {
			t := s.backendTag(host)
			_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%s\t%s", id, host, portStr, t))
			return
		}
	}

	// Sticky routing: honour an existing userDir entry if the backend is still Up
	// and matches the requested tag. Refreshes TTL so active users stay pinned.
	if e := s.userDir.Get(user); e != nil && !e.Weak {
		host, portStr, splitErr := net.SplitHostPort(e.Host)
		if splitErr == nil {
			if existing := s.ring.GetBackend(host); existing != nil && existing.Up {
				if existing.Tag == tag {
					s.userDir.Set(user, e.Host, false) // refresh TTL
					_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%s\t%s", id, host, portStr, existing.Tag))
					return
				}
			}
		}
	}

	// Ring lookup, always restricted to the requested tag ("" = untagged pool).
	b := s.ring.LookupBackendByTag(user, tag)
	if b == nil {
		_ = c.WriteLine(fmt.Sprintf("FAIL\t%s\treason=no-backends", id))
		return
	}

	// Record user→backend in directory and propagate the fresh sticky
	// assignment to the ring (#772 PR-2) so every director pins the user
	// to the SAME backend even where it diverges from the deterministic
	// hash (e.g. an entry that outlived a backend add/remove). Only the
	// NEW pin here propagates — the sticky TTL-refresh path above does
	// NOT, or every repeat login would be a ring broadcast. Sent
	// director↔director only (by hash), never to login clients, so it
	// bypasses originateRingEvent. The userDir Lamport stamp (assign_seq,
	// assign_by) rides in the payload; the ring envelope's own (origin,
	// seq) still dedups the propagation.
	addr = net.JoinHostPort(b.IP, strconv.Itoa(b.Port))
	h := s.userDir.Set(user, addr, false)
	if seq, by, ok := s.userDir.LastAssign(h); ok {
		s.membership.originate("USER-ASSIGN", fmt.Sprintf("%d\t%s\t%d\t%s", h, addr, seq, by))
	}

	_ = c.WriteLine(fmt.Sprintf("HOST\t%s\t%s\t%d\t%s", id, b.IP, b.Port, b.Tag))
}

// handleBackendUp processes: BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}
func (s *Server) handleBackendUp(c *client, fields []string) {
	if len(fields) < 3 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		_ = c.WriteLine("OK")
		return
	}
	tag := ""
	if len(fields) >= 4 {
		tag = fields[3]
	}
	vhosts := 0
	if len(fields) >= 5 {
		vhosts, _ = strconv.Atoi(fields[4])
	}
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: true, Vhosts: vhosts,
		LastUp: ts,
	})
	slog.Info("director: backend up", "ip", ip, "port", port, "tag", tag, "vhosts", vhosts)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tup\t%s", ip, tag), c)
	s.updateMetrics()
	_ = c.WriteLine("OK")
}

// handleBackendDown processes: BACKEND-DOWN\t{ip} and HOST-REMOVE\t{ip}
func (s *Server) handleBackendDown(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	tag := s.backendTag(ip)
	s.ring.RemoveBackend(ip)
	s.kickSessionsForBackend(ip)
	slog.Info("director: backend down", "ip", ip)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tdown\t%s", ip, tag), c)
	s.updateMetrics()
	_ = c.WriteLine("OK")
}

// handleBackendFlush processes: BACKEND-FLUSH\t{ip}
// Marks backend !Up so no new lookups are routed there, keeps it in the registry.
func (s *Server) handleBackendFlush(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	ip := fields[1]
	tag := s.backendTag(ip)
	if !s.ring.SetUp(ip, false, time.Now().Unix()) {
		slog.Warn("director: flush requested for unknown backend", "ip", ip)
		_ = c.WriteLine("OK")
		return
	}
	s.kickSessionsForBackend(ip)
	slog.Info("director: backend flush", "ip", ip)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tflush\t%s", ip, tag), c)
	s.updateMetrics()
	_ = c.WriteLine("OK")
}

// handleUserMove processes: USER-MOVE\t{user}\t{ip}\t{port}
func (s *Server) handleUserMove(c *client, fields []string) {
	if len(fields) < 4 {
		_ = c.WriteLine("OK")
		return
	}
	user, ip, portStr := s.normalizeUser(fields[1]), fields[2], fields[3]
	addr := net.JoinHostPort(ip, portStr)

	s.overrideMu.Lock()
	s.overrides[user] = addr
	s.overrideMu.Unlock()

	// Record in user directory as a strong assignment.
	s.userDir.Set(user, addr, false)

	slog.Info("director: user moved", "user", user, "backend", addr)
	s.originateRingEvent("USER-MOVED", fmt.Sprintf("%s\t%s\t%s", user, ip, portStr), c)
	_ = c.WriteLine("OK")
}

// handleUserRelease processes: USER-RELEASE\t{user}
func (s *Server) handleUserRelease(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := s.normalizeUser(fields[1])

	s.overrideMu.Lock()
	delete(s.overrides, user)
	s.overrideMu.Unlock()

	s.userDir.Delete(user)

	slog.Info("director: user released", "user", user)
	_ = c.WriteLine("OK")
}

// handleUserWeak processes: USER-WEAK\t{user}
// Marks the user's current directory entry as a soft/weak assignment.
// A weak assignment may be replaced if the user logs in from a different backend.
func (s *Server) handleUserWeak(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := s.normalizeUser(fields[1])
	e := s.userDir.Get(user)
	if e != nil {
		s.userDir.Set(user, e.Host, true)
	}
	slog.Info("director: user weakened", "user", user)
	_ = c.WriteLine("OK")
}

// handleUserKick processes: USER-KICK\t{user}
// Broadcasts a kick notification to all clients so login processes terminate
// existing sessions for this user.
func (s *Server) handleUserKick(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	user := s.normalizeUser(fields[1])
	slog.Info("director: user kick", "user", user)
	s.originateRingEvent("USER-KICKED", user, c)
	_ = c.WriteLine("OK")
}

// handleUserKilled processes: USER-KILLED\t{hash}
// A login client reports that all sessions for this user hash have been terminated.
// The director broadcasts USER-KILLED-EVERYWHERE to confirm completion.
func (s *Server) handleUserKilled(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	hashStr := fields[1]
	slog.Info("director: user killed", "hash", hashStr)
	s.broadcast(fmt.Sprintf("USER-KILLED-EVERYWHERE\t%s", hashStr), c)
	_ = c.WriteLine("OK")
}

// AddBackend registers a backend pod in the hash ring.
// Called at startup after DNS resolution of headless services.
func (s *Server) AddBackend(ip string, port int, tag string) {
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP:     ip,
		Port:   port,
		Tag:    tag,
		Up:     true,
		LastUp: ts,
	})
	slog.Info("director: backend registered", "ip", ip, "port", port, "tag", tag)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tup\t%s", ip, tag), nil)
	s.updateMetrics()
}

// LookupBackend returns the backend for the given username or nil if ring is empty.
func (s *Server) LookupBackend(username string) *ring.Backend {
	return s.ring.LookupBackend(username)
}

// RouteUser returns the backend IP for a recipient username, implementing
// lmtp.UserRouter. Checks admin overrides, sticky userDir, then the ring.
func (s *Server) RouteUser(username string) (string, error) {
	username = s.normalizeUser(username)
	s.overrideMu.RLock()
	addr, hasOverride := s.overrides[username]
	s.overrideMu.RUnlock()
	if hasOverride {
		host, _, err := net.SplitHostPort(addr)
		if err == nil {
			return host, nil
		}
	}
	if e := s.userDir.Get(username); e != nil && !e.Weak {
		host, _, err := net.SplitHostPort(e.Host)
		if err == nil {
			if b := s.ring.GetBackend(host); b != nil && b.Up {
				return host, nil
			}
		}
	}
	b := s.ring.LookupBackend(username)
	if b == nil {
		return "", fmt.Errorf("no backends available")
	}
	return b.IP, nil
}

// RecordUser writes a user→backend mapping into the user directory.
func (s *Server) RecordUser(username, backendAddr string) {
	s.userDir.Set(username, backendAddr, false)
}

// backendTag looks up the tag for a backend IP, returning "" if not found.
func (s *Server) backendTag(ip string) string {
	for _, b := range s.ring.Backends() {
		if b.IP == ip {
			return b.Tag
		}
	}
	return ""
}

// handleSessionOpen processes: SESSION-OPEN\t{id}\t{user}\t{backendIP}
func (s *Server) handleSessionOpen(c *client, fields []string) {
	if len(fields) < 4 {
		_ = c.WriteLine("OK")
		return
	}
	rec := &sessionRec{
		id:      fields[1],
		user:    fields[2],
		backend: fields[3],
		cl:      c,
	}
	s.sessRecMu.Lock()
	s.sessById[rec.id] = rec
	if s.sessByBE[rec.backend] == nil {
		s.sessByBE[rec.backend] = make(map[string]bool)
	}
	s.sessByBE[rec.backend][rec.id] = true
	s.sessRecMu.Unlock()
	_ = c.WriteLine("OK")
}

// handleSessionClose processes: SESSION-CLOSE\t{id}
func (s *Server) handleSessionClose(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	id := fields[1]
	s.sessRecMu.Lock()
	if rec, ok := s.sessById[id]; ok {
		delete(s.sessById, id)
		delete(s.sessByBE[rec.backend], id)
	}
	s.sessRecMu.Unlock()
	_ = c.WriteLine("OK")
}

// kickSessionsForBackend sends USER-KICKED to every login-pod connection that
// has an active session routed to backendIP, then removes those session records.
func (s *Server) kickSessionsForBackend(ip string) {
	s.sessRecMu.Lock()
	ids := s.sessByBE[ip]
	recs := make([]*sessionRec, 0, len(ids))
	for id := range ids {
		if rec, ok := s.sessById[id]; ok {
			recs = append(recs, rec)
			delete(s.sessById, id)
		}
	}
	delete(s.sessByBE, ip)
	s.sessRecMu.Unlock()

	for _, rec := range recs {
		_ = rec.cl.WriteLine(fmt.Sprintf("USER-KICKED\t%s", rec.user))
		slog.Info("director: kicked session", "session", rec.id, "user", rec.user, "backend", ip)
	}
}

// removeClientSessions removes all session records owned by a disconnected client.
func (s *Server) removeClientSessions(c *client) {
	s.sessRecMu.Lock()
	defer s.sessRecMu.Unlock()
	for id, rec := range s.sessById {
		if rec.cl == c {
			delete(s.sessById, id)
			delete(s.sessByBE[rec.backend], id)
		}
	}
}
