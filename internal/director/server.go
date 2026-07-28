// Package director implements the yarilo-director TCP+mTLS routing server.
// Login pods call it to resolve which backend pod should handle a given user,
// and health pods call it to register/deregister backends from the hash ring.
//
// Protocol (TAB-delimited, LF-terminated):
//
//	Server → Client handshake:
//	  VERSION\tyarilo-director\t1\t0\n
//	  HOST-HAND-START\n
//	  HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{hostname}\tV{vhosts}\n  (one per backend; V{vhosts} trailing, #706, tolerated-absent)
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
//	  LOOKUP\t{id}\t{user}\t{tag}\t{proto}\n           (tag required; ""=untagged pool. proto optional: base protocol for least_sessions #797)
//	  SESSION-OPEN\t{id}\t{user}\t{backendIP}\t{proto}\n  (proto optional, #797)
//	  SESSION-CLOSE\t{id}\n
//	  BACKEND-UP\t{ip}\t{port}\t{tag}\t{vhosts}\t{seq}\n  (vhosts optional; 0=100. seq optional: a backend's monotonic heartbeat counter, #776 — its presence makes the backend lease-managed / expirable)
//	  BACKEND-DOWN\t{ip}\n                            (LEAVE: remove + rehash — SIGTERM / expiry)
//	  BACKEND-FLUSH\t{ip}\n                          (drain / overload: stop new lookups, keep sessions + ring slot, NO rehash)
//	  BACKEND-UNREACHABLE\t{ip}\n                    (login proxy failed to dial {ip}; corroborated reports evict early — #782. Distinct from FLUSH: this is a down/rehash signal, not a drain)
//	  HOST-REMOVE\t{ip}\n                            (alias for BACKEND-DOWN)
//	  USER-MOVE\t{user}\t{ip}\t{port}\n              (move user: TTL'd userDir pin + kick old sessions, #708)
//	  USER-WEAK\t{user}\n                            (mark current assignment as soft/weak)
//	  USER-KICK\t{user}\n                            (broadcast kick to all login clients)
//	  USER-KILLED\t{hash}\n                          (login reports sessions closed for hash)
//	  PING\n
//	  QUIT\t{reason}\n
//
//	Server responses:
//	  HOST\t{id}\t{ip}\t{port}\t{tag}\n
//	  FAIL\t{id}\treason=no-backends\n
//	  FAIL\t{id}\treason=killing\n               (#847 — user is under a confirmed ring-wide kick; RETRYABLE, the login proxy re-LOOKUPs until the kill confirms rather than erroring the client)
//	  OK\n
//	  PONG\n
//
//	Server pushes (unsolicited, to local login clients — plain form, unchanged):
//	  RING-CHANGE\t{ip}\t{event}\t{tag}\n            (event: up | down | flush | vhosts)
//	    vhosts (#706): ...\t{ip}\tvhosts\t{tag}\t{count}\n — admin weight change,
//	    replicated ring-wide; carries NO seq, so it never turns the backend
//	    lease-managed.
//	  USER-MOVED\t{user}\t{ip}\t{port}\n             (when user is moved by another client)
//	  USER-KICKED\t{user}\n                          (broadcast kick notification)
//	  USER-KILLED-EVERYWHERE\t{hash}\n               (director confirms all sessions gone)
//
//	Server pushes (ring-envelope form, right-neighbor connections only, #750):
//	  RING-CHANGE\t{originIP}\t{originPort}\t{seq}\t{ip}\t{event}\t{tag}\n
//	    a lease-managed backend "up" carries port + vhosts so a director that
//	    only sees the backend via gossip (registration lands on 1 of N) can add
//	    it to its ring: ...\t{ip}\tup\t{tag}\t{beSeq}\t{port}\t{vhosts}\n (#776)
//	  USER-MOVED\t{originIP}\t{originPort}\t{seq}\t{user}\t{ip}\t{port}\n
//	  USER-KICKED\t{originIP}\t{originPort}\t{seq}\t{user}[\t{oldBackendIP}]\n
//	    oldBackendIP present (#708 move-kick): the pin is dropped only if it
//	    still points there (compare-and-delete). Absent (#823 admin kick): the
//	    pin is dropped unconditionally. The session kick fires either way.
//	  SESSION-OPEN\t{originIP}\t{originPort}\t{seq}\t{id}\t{user}\t{backend}\t{proto}\n  (#804 — replicate the load view)
//	  SESSION-CLOSE\t{originIP}\t{originPort}\t{seq}\t{id}\n
//	  USER-KILLING\t{originIP}\t{originPort}\t{seq}\t{hash}\t{ttlMillis}\n
//	    (#847 — a user entered a confirmed kick; hold LOOKUP ring-wide. ttlMillis
//	    is a DURATION: each director computes its own deadline on receipt, never a
//	    wall-clock deadline, which pod-clock skew would make unstable)
//	  USER-KILL-DONE\t{originIP}\t{originPort}\t{seq}\t{hash}\n
//	    (#847 — kill confirmed (sessions gone) or timed out; release the hold)
//	  BACKEND-UNREACHABLE\t{originIP}\t{originPort}\t{seq}\t{backendIP}\t{reporterID}\n
//	    (#782 — replicate a proxy's unreachable report ring-wide so corroboration
//	    aggregates across directors. reporterID is the reporting login proxy, in
//	    the payload so a gossip copy counts under the ORIGINAL reporter, never as
//	    a new one — the seq/originIP identify the relaying director, not the proxy)
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

	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
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
	// WriteTimeout bounds a single push/response write to a client (#704) so
	// one slow/stuck client cannot block the broadcast fan-out (and, formerly,
	// hold the client read-lock) and stall the push plane for everyone. 0 =
	// default 10s; negative = no deadline (legacy blocking behaviour).
	WriteTimeout time.Duration
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
	// JoinAllowedNets restricts which source CIDRs a DIRECTOR-JOIN is accepted
	// from (#773). Nil/empty = allow all. Checked before the HMAC challenge.
	JoinAllowedNets []*net.IPNet
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
	// BackendExpire is how long a lease-managed backend may go without a
	// heartbeat before it is removed ring-wide (#776). A backend becomes
	// lease-managed when a seq'd BACKEND-UP arrives for it; static
	// mail_servers / admin-added backends never heartbeat and are never
	// expired. 0 = default (30s); negative = disabled (no lease expiry).
	BackendExpire time.Duration
	// UnreachableReporters is how many DISTINCT login proxies must report a
	// backend unreachable within UnreachableWindow before it is evicted from
	// the ring ahead of the lease TTL (#782). 0 = default (2).
	UnreachableReporters int
	// UnreachableWindow is the sliding window over which those distinct reports
	// must arrive. 0 = default (5s).
	UnreachableWindow time.Duration
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
	// account route to the same backend. Defaults to true. Legacy knob: when
	// UsernameHashFormat is set it takes over case-folding and this is ignored.
	UsernameHashLowercase *bool
	// UsernameHashFormat is the username→hash-key template (director_service.username_hash,
	// #850), mirroring the reference director_username_hash expression: %u (whole user),
	// %n (local part), %d (domain), each with optional %L lowercase, plus %%. Empty derives
	// the template from UsernameHashLowercase (%Lu / %u) for byte-identical back-compat.
	// Parsed once at startup; an invalid template is rejected in main before the server
	// is built.
	UsernameHashFormat string
	// AssignmentPolicy selects the INITIAL (unpinned) placement strategy (#797):
	// "hash" (default, Dovecot semantics) or "least_sessions" (load-aware).
	// Sticky pins / USER-MOVE are unaffected.
	AssignmentPolicy string
	// UserKickDelay delays an admin-initiated kick before the USER-KICKED is
	// pushed (#740), a grace window for a user's in-flight command on the old
	// backend after a move. Admin path only — backend-down/expiry and the
	// split-writer conflict-kick are never delayed. 0 = 2s; negative = 0.
	UserKickDelay time.Duration
	// MaxParallelKicks caps sessions kicked per batch on backend-down (#740),
	// spreading the re-login stampede. 0 = 100; <= 0 after default = no batching.
	MaxParallelKicks int
	// MaxParallelMoves caps concurrent user moves during a graceful evacuation
	// (#849). 0 = 5; negative = unlimited.
	MaxParallelMoves int
	// FlushProgram is an optional per-user cleanup hook run after a confirmed move
	// (#848); empty = disabled. See config.DirectorServiceConfig.FlushProgram.
	FlushProgram string
	// UserKillTimeout is the hard fallthrough for the confirmed kick (#847);
	// UserKillConfirmGrace is the stable-zero window before confirming. 0 =
	// defaults (15s, 1s).
	UserKillTimeout      time.Duration
	UserKillConfirmGrace time.Duration
}

func (o *Options) userKillTimeout() time.Duration {
	if o.UserKillTimeout <= 0 {
		return 15 * time.Second
	}
	return o.UserKillTimeout
}

func (o *Options) userKillConfirmGrace() time.Duration {
	if o.UserKillConfirmGrace <= 0 {
		return 1 * time.Second
	}
	return o.UserKillConfirmGrace
}

func (o *Options) userKickDelay() time.Duration {
	if o.UserKickDelay == 0 {
		return 2 * time.Second
	}
	if o.UserKickDelay < 0 {
		return 0
	}
	return o.UserKickDelay
}

func (o *Options) maxParallelKicks() int {
	if o.MaxParallelKicks == 0 {
		return 100
	}
	if o.MaxParallelKicks < 0 {
		return 0
	}
	return o.MaxParallelKicks
}

// maxParallelMoves returns the graceful-evacuation concurrency window (#849).
// 0 = default 5; negative = unlimited (returned as 0, interpreted as "no ceiling").
func (o *Options) maxParallelMoves() int {
	if o.MaxParallelMoves == 0 {
		return 5
	}
	if o.MaxParallelMoves < 0 {
		return 0
	}
	return o.MaxParallelMoves
}

func (o *Options) usernameHashLowercase() bool {
	if o.UsernameHashLowercase == nil {
		return true
	}
	return *o.UsernameHashLowercase
}

// effectiveHashFormat resolves the username→hash-key template (#850). An explicit
// UsernameHashFormat wins and reports explicit=true; otherwise the template is derived
// from the legacy usernameHashLowercase bool (%Lu / %u) so pre-#850 configs hash
// byte-for-byte as before and explicit=false. When explicit, ingress normalizeUser
// becomes a no-op and the template's %L is the sole case-folder. A malformed explicit
// template is unreachable here — main validates it and fails loudly before the server is
// built — so the parse error falls back to the safe default rather than panicking.
func (o *Options) effectiveHashFormat() (hf ring.HashFormat, explicit bool) {
	if raw := strings.TrimSpace(o.UsernameHashFormat); raw != "" {
		if parsed, err := ring.ParseHashFormat(raw); err == nil {
			return parsed, true
		}
	}
	tmpl := "%Lu"
	if !o.usernameHashLowercase() {
		tmpl = "%u"
	}
	parsed, _ := ring.ParseHashFormat(tmpl)
	return parsed, false
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

// writeTimeout is the per-write deadline (#704). 0 = 10s; negative = disabled.
func (o *Options) writeTimeout() time.Duration {
	if o.WriteTimeout == 0 {
		return 10 * time.Second
	}
	if o.WriteTimeout < 0 {
		return 0
	}
	return o.WriteTimeout
}

// client wraps an active connection with a per-connection write lock. The lock
// guarantees each written LINE is atomic — it does NOT order a push relative to
// a command reply, so a push can still land between a request and its reply on
// the same conn; the request/reply reader (proto.Conn.readReply, #702) skips
// such interleaved pushes rather than relying on ordering here.
type client struct {
	conn         net.Conn
	mu           sync.Mutex
	writeTimeout time.Duration // per-write deadline (#704); 0 = none
	pongCh       chan struct{} // receives a token each time PONG is received
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
	if c.writeTimeout > 0 {
		// Bound this write so a stuck client can't block the broadcaster (#704).
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		defer c.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck
	}
	_, err := io.WriteString(c.conn, line+"\n")
	return err
}

// sessionRec tracks one active proxied session reported by a login-pod.
type sessionRec struct {
	id      string
	user    string
	backend string  // backend IP (without port)
	proto   string  // base protocol (imap/pop3/…), for least_sessions counts (#797)
	cl      *client // which login-pod connection owns this session
}

// Server is the yarilo-director TCP server.
type Server struct {
	ring *ring.Ring
	opts Options

	// hf is the compiled username→hash-key template (#850), shared verbatim with s.ring
	// and s.userDir so every hash of a user goes through one code path. hashFmtExplicit
	// records whether the operator set username_hash explicitly; when true, ingress
	// normalizeUser is a no-op and hf's %L is the sole case-folder.
	hf              ring.HashFormat
	hashFmtExplicit bool

	// userDir stores user→backend mappings with TTL.
	userDir *UserDir

	// clients is the registry of all currently connected clients.
	clientMu sync.RWMutex
	clients  map[*client]struct{}

	// sessRec tracks active proxied sessions reported via SESSION-OPEN/CLOSE.
	// Director uses this to send USER-KICKED when a backend goes down.
	sessRecMu sync.RWMutex
	sessById  map[string]*sessionRec     // sessionID → record
	sessByBE  map[string]map[string]bool // backendIP → set of sessionIDs

	// membership owns the self-organizing ring: member list, the single
	// right-neighbor connection, and (origin, seq) event forwarding (#750).
	membership *Membership

	// backendSeen tracks the heartbeat lease per backend (#776): a backend
	// becomes lease-managed the first time a seq'd BACKEND-UP arrives for
	// it (directly or gossiped). Static mail_servers / admin-added backends
	// never heartbeat, so they never enter this map and are never expired.
	// A lease-managed backend whose last-seen has not advanced within
	// BackendExpire is removed ring-wide.
	backendSeenMu sync.Mutex
	backendSeen   map[string]backendLease

	// unreach tracks corroborated backend-unreachable reports (#782):
	// backendIP -> reporterID (login-proxy identity) -> latest report time.
	// Reports replicate ring-wide (the count must aggregate across directors,
	// #804), so a gossiped copy is recorded under the ORIGINAL reporter and
	// never counts as a second one. Distinct reporters within the window that
	// reach the threshold trigger an early eviction.
	unreachMu sync.Mutex
	unreach   map[string]map[string]time.Time

	// backendTomb records recently-removed backends (down / expiry / unreachable
	// eviction) and the lease seq at removal (#846), so a resync snapshot from a
	// peer that has not yet learned of the removal cannot resurrect the ghost —
	// only a strictly-newer seq (a genuine re-registration) re-admits it.
	backendTombMu sync.Mutex
	backendTomb   map[string]backendTombstone

	// killing tracks users under a confirmed ring-wide kick (#847), keyed by
	// username hash. While a user is killing, LOOKUP is held (no fresh backend
	// assignment) so a concurrent login cannot open on a new backend before the
	// old sessions are gone. Cleared when the user's ring-wide session count has
	// stayed at zero for the confirm grace, or when the hard timeout elapses.
	killMu  sync.Mutex
	killing map[uint32]killState

	// evacMu guards evac, the set of in-progress graceful backend drains (#849),
	// keyed by backend IP. Each drain is a resumable, throttled cursor advanced
	// from the confirmed-kill sweep (evacKillDone).
	evacMu sync.Mutex
	evac   map[string]*evacuation

	// apiToken / apiAddr are captured when StartAPI runs so the cross-replica
	// ring-topology aggregator (#833 PR-B) can fan out to peers' own admin APIs
	// with the shared per-release token, deriving each peer's API endpoint from
	// its ring IP + this replica's api.listen port (uniform-api.listen
	// assumption — see apiRingTopology).
	apiToken string
	apiAddr  string
}

// backendLease records the freshest heartbeat seen for a backend and the
// LOCAL time this director saw it (#776). seq is the backend's own
// monotonic per-origin counter — compared only within that backend's
// origin (the Lamport lesson of #772), never across backends. at is used
// for time-based expiry.
type backendLease struct {
	seq uint64
	at  time.Time
}

// New creates a director server with an empty ring and default options.
func New() *Server {
	return NewWithOptions(Options{})
}

// NewWithOptions creates a director server with custom options.
func NewWithOptions(opts Options) *Server {
	hf, hfExplicit := opts.effectiveHashFormat()
	s := &Server{
		ring:            ring.New(hf),
		opts:            opts,
		hf:              hf,
		hashFmtExplicit: hfExplicit,
		userDir:         NewUserDir(opts.userExpire(), hf, Member{IP: opts.LocalIP, Port: opts.LocalPort}.String()),
		clients:         make(map[*client]struct{}),
		sessById:        make(map[string]*sessionRec),
		sessByBE:        make(map[string]map[string]bool),
		backendSeen:     make(map[string]backendLease),
		unreach:         make(map[string]map[string]time.Time),
		backendTomb:     make(map[string]backendTombstone),
		killing:         make(map[uint32]killState),
		evac:            make(map[string]*evacuation),
	}
	s.membership = NewMembership(s, Member{IP: opts.LocalIP, Port: opts.LocalPort}, opts.RingSecret, opts.PeerTLS, opts.MinMembers, opts.JoinAllowedNets)
	s.membership.antiEntropyInterval = opts.AntiEntropyInterval
	s.membership.seedPollInterval = opts.SeedPollInterval
	s.membership.seedPollIdleInterval = opts.SeedPollIdleInterval
	s.membership.tombstoneTTL = opts.TombstoneTTL
	return s
}

// normalizeUser applies the #738 username hash-normalization at the single
// ingress point for every wire/HTTP handler that turns a raw username into a
// hash or map key (handleLookup, handleUserMove, handleUserWeak,
// handleUserKick, RouteUser, apiUserMove, apiUserKick).
// Everything downstream of these call sites — s.userDir,
// s.ring — already operates on normalized usernames, so no other call site
// needs its own normalize call.
//
// Ordering with #701 (LOOKUP field TAB-escaping): the wire senders that escape
// the username (proto.Conn.Lookup, proto.Conn.SessionOpen) are unescaped at
// their ingress BEFORE this normalize runs — normalizeUser(proto.TabUnescape(raw))
// in handleLookup — since escaping is reversible byte-for-byte and lowercasing
// is not. Admin/API ingress (apiUserMove/apiUserKick) is NOT tab-escaped and
// must not be unescaped.
func (s *Server) normalizeUser(username string) string {
	// #850: with an explicit username_hash template, all case-folding lives in the
	// template (%L), applied inside the hash on both the ring and userDir sides — so
	// ingress must NOT pre-lowercase, or a case-sensitive %u would be defeated. Back-compat
	// (no explicit template) keeps the #738 bool-driven ingress lowercasing, which is
	// byte-identical to the derived %Lu/%u hashing.
	if s.hashFmtExplicit {
		return username
	}
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

// GracefulLeave announces this director's exit to the ring (#770) — call
// on SIGTERM, BEFORE cancelling the server ctx, then allow a brief flush.
// See Membership.Leave.
func (s *Server) GracefulLeave() { s.membership.Leave() }

// ListPeers returns the current ring membership (self included), formatted
// as "ip:port" strings — kept for API/CLI compatibility (yarctl
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
	// Snapshot the targets under the lock, then write OUTSIDE it (#704): a
	// bounded but slow write must not hold clientMu and block registrations.
	s.clientMu.RLock()
	targets := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		if c != exclude {
			targets = append(targets, c)
		}
	}
	s.clientMu.RUnlock()
	for _, c := range targets {
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
	targets := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		if !c.isPeer {
			targets = append(targets, c)
		}
	}
	s.clientMu.RUnlock()
	for _, c := range targets {
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
	targets := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		if c != exclude && !c.isPeer {
			targets = append(targets, c)
		}
	}
	s.clientMu.RUnlock()
	for _, c := range targets {
		_ = c.WriteLine(kind + "\t" + payload)
	}
	s.membership.originate(kind, payload)
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	c := &client{conn: conn, writeTimeout: s.opts.writeTimeout(), pongCh: make(chan struct{}, 4)}
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
		case "BACKEND-UNREACHABLE":
			s.handleBackendUnreachable(c, fields)
		case "USER-MOVE":
			s.handleUserMove(c, fields)
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
	// V{vhosts} is a trailing field (#706): a joining director learns each
	// backend's ring weight from the handshake, not only its up/down state.
	// applyHandshakeHost tolerates its absence, so a pre-#706 peer still parses.
	return fmt.Sprintf("HOST\t%s\t%d\t%s\tD%d\tU%d\t%s\tV%d",
		b.IP, b.Port, b.Tag, b.LastDown, b.LastUp, b.Hostname, b.Vhosts)
}

// handleLookup processes: LOOKUP\t{id}\t{user}\t{tag}
// tag is required; "" restricts to the untagged pool — there is no
// full-ring mode (#737: a login pod belongs to exactly one tag-pool, per
// DEPLOYMENT.md's tag-based sharding model).
// One order (#708): sticky userDir pin, then ring lookup restricted to tag.
// Response: HOST\t{id}\t{ip}\t{port}\t{tag}
func (s *Server) handleLookup(c *client, fields []string) {
	if len(fields) < 4 {
		return
	}
	id, user := fields[1], s.normalizeUser(proto.TabUnescape(fields[2]))
	tag := fields[3]
	reqProto := "" // base protocol of the requesting login (#797), optional
	if len(fields) >= 5 {
		reqProto = fields[4]
	}

	// Confirmed-kick hold (#847): while this user is being killed ring-wide,
	// refuse to assign a backend so a concurrent login cannot open on a fresh
	// pod before the old sessions are gone (the split-writer window). This gates
	// only fresh LOOKUP assignment — the move's own replicated USER-ASSIGN pin
	// is applied elsewhere and is unaffected. The reason is retryable: the login
	// proxy re-LOOKUPs until the kill confirms, rather than erroring the client.
	if s.isKilling(HashUsername(user, s.hf)) {
		_ = c.WriteLine(fmt.Sprintf("FAIL\t%s\treason=%s", id, killReason))
		return
	}

	// One lookup order (#708): sticky userDir pin → ring. An admin USER-MOVE now
	// writes a normal (TTL'd) userDir entry, so there is no separate override
	// branch to check first.
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

	// Fresh assignment via the configured policy (hash | least_sessions, #797),
	// restricted to the requested tag ("" = untagged pool). assignAndPin is the
	// single owner: it picks, records the sticky pin, and propagates USER-ASSIGN
	// (#772 PR-2) so every director pins the user to the SAME backend — the same
	// machinery whether the pick was a hash or a least-loaded choice. Sent
	// director↔director only (by hash), never to login clients.
	b := s.assignAndPin(user, tag, reqProto)
	if b == nil {
		_ = c.WriteLine(fmt.Sprintf("FAIL\t%s\treason=no-backends", id))
		return
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
	// Optional 6th field: the backend's monotonic heartbeat seq (#776). Its
	// presence marks this a lease-managed backend (a self-registering pod);
	// its absence keeps the legacy non-expiring behaviour (static
	// mail_servers seeding, admin tooling). A stale/duplicate heartbeat
	// (seq not newer than what we already recorded) is not re-gossiped.
	var seq uint64
	hasSeq := false
	if len(fields) >= 6 {
		if v, err := strconv.ParseUint(fields[5], 10, 64); err == nil {
			seq, hasSeq = v, true
		}
	}
	if hasSeq && !s.recordBackendSeen(ip, seq) {
		// Duplicate/stale heartbeat: refresh nothing, don't re-gossip.
		_ = c.WriteLine("OK")
		return
	}
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP: ip, Port: port, Tag: tag, Up: true, Vhosts: vhosts,
		LastUp: ts,
	})
	// A fresh (re)admit clears any stale unreachable reports (#782): a backend
	// that heartbeats again was a transient blip, so it must not be re-evicted
	// by reports from before it recovered — a new corroboration must build up.
	s.clearUnreachable(ip)
	s.clearBackendTomb(ip) // #846: a genuine re-registration drops the resync tombstone
	slog.Info("director: backend up", "ip", ip, "port", port, "tag", tag, "vhosts", vhosts, "seq", seq)
	payload := fmt.Sprintf("%s\tup\t%s", ip, tag)
	if hasSeq {
		// Carry port + vhosts so a director that receives this gossip WITHOUT
		// a persistent connection from the backend (the ClusterIP registration
		// lands on exactly one of N directors) can add the backend to its ring
		// for routing — SetUp alone can't, it has no port. seq stays at field 3
		// so a legacy seq-only payload keeps parsing.
		payload = fmt.Sprintf("%s\tup\t%s\t%d\t%d\t%d", ip, tag, seq, port, vhosts)
	}
	s.originateRingEvent("RING-CHANGE", payload, c)
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
	s.recordBackendTomb(ip) // #846: block resync resurrection until a newer seq
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
	// Wire BACKEND-FLUSH is a backend self-reporting overload (#779/#811):
	// DRAIN, not evacuate. Keep the ring slot and every existing session; only
	// clear the pins so NEW lookups rehash away. Do NOT kick — the wire-doc
	// contract is "keep sessions, no rehash". (Operator-forced evacuation with a
	// kick is the admin `backends flush` command, apiBackendFlush.)
	n := s.userDir.DeleteByBackend(ip)
	slog.Info("director: backend flush (drain)", "ip", ip, "pins_cleared", n)
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
	user := s.normalizeUser(fields[1])
	s.moveUser(user, net.JoinHostPort(fields[2], fields[3]), c)
	_ = c.WriteLine("OK")
}

// moveUser records an admin-forced move as a normal (TTL'd) userDir pin and
// kicks the user's sessions on the OLD backend so the next connection lands on
// the new one (#708) — move = route change + kick, atomically. The kick carries
// the old backend IP: the compare-and-delete apply drops the pin only if it
// still points there, so this move's fresh pin survives the kick it triggers.
// A plain admin kick (apiUserKick, no old IP) still clears unconditionally
// (#823). Longevity is not permanent — while the user has a live session the
// pin's TTL is kept fresh (#804 registry, PR-B); an idle user falls back to the
// ring hash after user_expire. exclude is the originating client (nil for API).
func (s *Server) moveUser(user, addr string, exclude *client) {
	oldIP := ""
	if e := s.userDir.Get(user); e != nil {
		if h, _, err := net.SplitHostPort(e.Host); err == nil {
			oldIP = h
		}
	}
	s.userDir.Set(user, addr, false)
	host, portStr, _ := net.SplitHostPort(addr)
	s.originateRingEvent("USER-MOVED", fmt.Sprintf("%s\t%s\t%s", user, host, portStr), exclude)
	if oldIP != "" && oldIP != host {
		// A genuine relocation kicks the old sessions — same split-writer window
		// as an admin kick, so hold LOOKUP until the old sessions confirm gone
		// (#847). The new pin above is already set and is NOT gated (the hold is
		// only on fresh LOOKUP assignment); a held login re-LOOKUPs and lands on
		// the new pin once the kill confirms.
		hash := HashUsername(user, s.hf)
		s.startKilling(hash)
		// #848: run the operator flush hook once this move confirms (old sessions
		// gone). Only this originating director attaches the context.
		s.attachFlush(hash, flushCtx{user: user, oldBackend: oldIP, newBackend: host})
		// Conditional kick: USER-KICKED with a trailing old-backend field.
		s.originateRingEvent("USER-KICKED", fmt.Sprintf("%s\t%s", user, oldIP), exclude)
	}
	slog.Info("director: user moved", "user", user, "backend", addr, "kicked_old", oldIP)
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
	// Mark the user killing (and replicate it) BEFORE the kick tears sessions
	// down, so the LOOKUP hold is in force ring-wide for the whole drain (#847).
	s.startKilling(HashUsername(user, s.hf))
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
func (s *Server) AddBackend(ip string, port int, tag string, vhosts int) {
	ts := time.Now().Unix()
	s.ring.AddBackend(&ring.Backend{
		IP:     ip,
		Port:   port,
		Tag:    tag,
		Up:     true,
		Vhosts: vhosts,
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
// lmtp.UserRouter. One order (#708): sticky userDir pin → ring.
func (s *Server) RouteUser(username string) (string, error) {
	username = s.normalizeUser(username)
	if e := s.userDir.Get(username); e != nil && !e.Weak {
		host, _, err := net.SplitHostPort(e.Host)
		if err == nil {
			if b := s.ring.GetBackend(host); b != nil && b.Up {
				return host, nil
			}
		}
	}
	// Fresh (unpinned) delivery. Under least_sessions the placement must be the
	// director's single owned decision (assignAndPin), or LMTP could pick a
	// different pod than a concurrent IMAP login and split the user's writer
	// (#797/#788). Under hash the deterministic lookup stays read-only.
	if s.assignmentPolicy() == policyLeastSessions {
		if b := s.assignAndPin(username, "", "lmtp"); b != nil {
			return b.IP, nil
		}
		return "", fmt.Errorf("no backends available")
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
		id: fields[1],
		// SESSION-OPEN escapes the username on the wire (proto.Conn.SessionOpen),
		// exactly like LOOKUP — unescape so the username echoed back in
		// USER-KICKED matches what the login proxy sent (#701). No normalize:
		// this field is not a hash key, and lowercasing it would break the
		// login-side kick match on the original-case username.
		user:    proto.TabUnescape(fields[2]),
		backend: fields[3],
		cl:      c,
	}
	if len(fields) >= 5 {
		rec.proto = fields[4] // base protocol (#797), optional
	}
	s.sessRecMu.Lock()
	s.sessById[rec.id] = rec
	if s.sessByBE[rec.backend] == nil {
		s.sessByBE[rec.backend] = make(map[string]bool)
	}
	s.sessByBE[rec.backend][rec.id] = true
	s.sessRecMu.Unlock()
	// Replicate ring-wide (#804): SESSION-OPEN/CLOSE land on ONE director (the
	// login pod's watch-holder behind the ClusterIP), but least_sessions (#797)
	// needs the cluster-wide session view on whichever RANDOM replica answers a
	// LOOKUP. Gossip it as an (origin, seq) envelope like USER-ASSIGN; peers add
	// a REMOTE record (cl=nil). Kick stays local (owning conn), so remote records
	// only feed the load view.
	s.membership.originate("SESSION-OPEN", fmt.Sprintf("%s\t%s\t%s\t%s", rec.id, proto.TabEscape(rec.user), rec.backend, rec.proto))
	s.noteSessionOpened(rec.user) // #847: a new session voids a pending kill-confirm
	s.updateMetrics()             // refresh backend_sessions
	_ = c.WriteLine("OK")
}

// handleSessionClose processes: SESSION-CLOSE\t{id}
func (s *Server) handleSessionClose(c *client, fields []string) {
	if len(fields) < 2 {
		_ = c.WriteLine("OK")
		return
	}
	id := fields[1]
	closedUser := ""
	s.sessRecMu.Lock()
	if rec, ok := s.sessById[id]; ok {
		closedUser = rec.user
		delete(s.sessById, id)
		delete(s.sessByBE[rec.backend], id)
	}
	s.sessRecMu.Unlock()
	s.membership.originate("SESSION-CLOSE", id) // drop the remote copies (#804)
	if closedUser != "" {
		s.noteSessionClosed(closedUser) // #847: arm kill-confirm when the count hits zero
	}
	s.updateMetrics() // refresh backend_sessions
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

	// A backend-down removal also drops these sessions from the ring-wide count,
	// so arm the kill-confirm for any of these users under a kill (#847). Cheap
	// for the common case: noteSessionClosed early-returns unless the user is
	// actually killing.
	for _, rec := range recs {
		s.noteSessionClosed(rec.user)
	}

	// Only local sessions carry a conn to kick; remote replicas (#804) are the
	// owning director's job — every director runs this on the gossiped
	// backend-down, so each kicks its own local sessions.
	local := recs[:0]
	for _, rec := range recs {
		if rec.cl != nil {
			local = append(local, rec)
		}
	}
	if len(local) == 0 {
		return
	}

	kickOne := func(rec *sessionRec) {
		_ = rec.cl.WriteLine(fmt.Sprintf("USER-KICKED\t%s", rec.user))
		slog.Info("director: kicked session", "session", rec.id, "user", rec.user, "backend", ip)
	}

	batch := s.opts.maxParallelKicks()
	if batch <= 0 || batch >= len(local) {
		for _, rec := range local {
			kickOne(rec)
		}
		return
	}

	// Batch to spread the re-login stampede (#740): kick maxParallelKicks at a
	// time with a short pause between batches so surviving backends don't take
	// every re-login at once. Off the caller goroutine so the backend-down
	// handler isn't blocked by the paced kicks.
	go func() {
		for i := 0; i < len(local); i += batch {
			end := i + batch
			if end > len(local) {
				end = len(local)
			}
			for _, rec := range local[i:end] {
				kickOne(rec)
			}
			if end < len(local) {
				time.Sleep(kickBatchPause)
			}
		}
	}()
}

// kickBatchPause is the delay between successive max_parallel_kicks batches
// (#740). Fixed rather than configurable: it only shapes how the re-login
// stampede is spread, and max_parallel_kicks is the operator-facing dial.
const kickBatchPause = 100 * time.Millisecond

// recordBackendSeen records a heartbeat for a backend under the #776 lease:
// it refreshes the local last-seen time only on a STRICTLY newer per-origin
// seq (a gossiped duplicate of a heartbeat already recorded does not extend
// the lease; a stale/lower seq is ignored). Returns true when the seq was
// fresh — the caller then gossips it onward so every director's lease for
// this backend is refreshed by a heartbeat that landed on ANY of them.
func (s *Server) recordBackendSeen(ip string, seq uint64) bool {
	s.backendSeenMu.Lock()
	defer s.backendSeenMu.Unlock()
	cur, ok := s.backendSeen[ip]
	if ok && seq <= cur.seq {
		return false
	}
	s.backendSeen[ip] = backendLease{seq: seq, at: time.Now()}
	return true
}

// forgetBackendLease drops a backend's lease (on removal), so a later
// re-registration starts a fresh lease rather than comparing against a
// stale seq.
func (s *Server) forgetBackendLease(ip string) {
	s.backendSeenMu.Lock()
	delete(s.backendSeen, ip)
	s.backendSeenMu.Unlock()
}

func (o *Options) backendExpire() time.Duration {
	if o.BackendExpire == 0 {
		return 30 * time.Second
	}
	return o.BackendExpire
}

// StartBackendExpiry runs the #776 lease-expiry loop until ctx ends: every
// backendExpire/3 it removes any lease-managed backend whose heartbeat has
// not advanced within backendExpire (silent hang / crash), ring-wide via
// RING-CHANGE down. Never expires the LAST backend of a tag — a
// suspect-but-only backend is kept (logged loudly) over a guaranteed total
// blackhole. Negative BackendExpire disables the loop.
// StartSessionRefresh keeps a user's sticky pin alive while they have a live
// proxied session (#708 PR-B, session-registry-driven — no #707 login refresh).
// Every user_expire/2 it touches the userDir pin of every user with an active
// session in the #804 registry, so a moved/assigned user does not TTL-expire
// mid-session; when their last session closes the touches stop and the pin
// lapses back to the ring hash after user_expire. Each director refreshes from
// its OWN (ring-replicated) session view, so no extra propagation is needed.
func (s *Server) StartSessionRefresh(ctx context.Context) {
	exp := s.opts.userExpire()
	tick := exp / 2
	if tick < time.Second {
		tick = time.Second
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshPinnedSessions()
			}
		}
	}()
}

// refreshPinnedSessions touches the pin of every user with a live session.
func (s *Server) refreshPinnedSessions() {
	s.sessRecMu.RLock()
	users := make(map[string]struct{}, len(s.sessById))
	for _, rec := range s.sessById {
		if rec.user != "" {
			users[rec.user] = struct{}{}
		}
	}
	s.sessRecMu.RUnlock()
	for u := range users {
		s.userDir.Touch(u)
	}
}

func (s *Server) StartBackendExpiry(ctx context.Context) {
	if s.opts.BackendExpire < 0 {
		return
	}
	exp := s.opts.backendExpire()
	tick := exp / 3
	if tick < time.Second {
		tick = time.Second
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.expireStaleBackends(exp)
			}
		}
	}()
}

func (s *Server) expireStaleBackends(expire time.Duration) {
	now := time.Now()
	s.backendSeenMu.Lock()
	var stale []string
	for ip, lease := range s.backendSeen {
		if now.Sub(lease.at) > expire {
			stale = append(stale, ip)
		}
	}
	s.backendSeenMu.Unlock()

	for _, ip := range stale {
		b := s.ring.GetBackend(ip)
		if b == nil {
			s.forgetBackendLease(ip)
			continue
		}
		if s.ring.CountBackendsInTag(b.Tag) <= 1 {
			slog.Warn("director: backend lease expired but it is the LAST in its tag — keeping to avoid total blackhole",
				"ip", ip, "tag", b.Tag)
			continue
		}
		s.recordBackendTomb(ip) // #846: before forgetBackendLease, which clears the seq
		s.forgetBackendLease(ip)
		s.ring.RemoveBackend(ip)
		s.kickSessionsForBackend(ip)
		s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tdown\t%s", ip, b.Tag), nil)
		s.updateMetrics()
		slog.Warn("director: backend expired, no heartbeat", "ip", ip, "tag", b.Tag, "expire", expire)
	}
}

// kickStaleSessions kicks this director's own sessions for user `hash` that
// are still routed to oldHost after a ring merge moved the user to a
// different backend (#772 PR-3) — a genuine two-replica conflict (lower id
// won, we lost) or any newer reassignment. Leaving them would split-brain
// the mailbox across two backends. Only sessions whose username hashes to
// `hash` are touched; other users on the same backend keep running. oldHost
// is "ip:port"; sessions are keyed by backend IP.
func (s *Server) kickStaleSessions(hash uint32, oldHost string) {
	ip, _, err := net.SplitHostPort(oldHost)
	if err != nil {
		ip = oldHost
	}
	s.sessRecMu.Lock()
	var victims []*sessionRec
	for id := range s.sessByBE[ip] {
		if rec, ok := s.sessById[id]; ok && HashUsername(rec.user, s.hf) == hash {
			victims = append(victims, rec)
			delete(s.sessById, id)
			delete(s.sessByBE[ip], id)
		}
	}
	s.sessRecMu.Unlock()

	for _, rec := range victims {
		_ = rec.cl.WriteLine(fmt.Sprintf("USER-KICKED\t%s", rec.user))
		slog.Info("director: kicked stale session after ring reassignment", "session", rec.id, "user", rec.user, "old_backend", ip)
	}
}

// removeClientSessions removes all session records owned by a disconnected
// client and gossips a SESSION-CLOSE for each so peers drop their remote copies
// (#804) — a login pod dropping without a clean SESSION-CLOSE must not leave its
// sessions counted forever on every replica.
func (s *Server) removeClientSessions(c *client) {
	s.sessRecMu.Lock()
	var closedIDs []string
	for id, rec := range s.sessById {
		if rec.cl == c {
			delete(s.sessById, id)
			delete(s.sessByBE[rec.backend], id)
			closedIDs = append(closedIDs, id)
		}
	}
	s.sessRecMu.Unlock()
	for _, id := range closedIDs {
		s.membership.originate("SESSION-CLOSE", id)
	}
	if len(closedIDs) > 0 {
		s.updateMetrics()
	}
}

// applyRemoteSessionOpen records a session reported by ANOTHER director via the
// ring (#804). Marked remote (cl=nil): it feeds the least_sessions load view but
// is never kicked locally (that is the owning director's job).
func (s *Server) applyRemoteSessionOpen(payload []string) {
	if len(payload) < 3 {
		return
	}
	rec := &sessionRec{id: payload[0], user: proto.TabUnescape(payload[1]), backend: payload[2]}
	if len(payload) >= 4 {
		rec.proto = payload[3]
	}
	s.sessRecMu.Lock()
	// Never clobber a locally-owned record (ids are per-login-pod unique, so this
	// is defensive) — keep ours so kick still has the owning conn.
	if ex, ok := s.sessById[rec.id]; ok && ex.cl != nil {
		s.sessRecMu.Unlock()
		return
	}
	s.sessById[rec.id] = rec
	if s.sessByBE[rec.backend] == nil {
		s.sessByBE[rec.backend] = make(map[string]bool)
	}
	s.sessByBE[rec.backend][rec.id] = true
	s.sessRecMu.Unlock()
	s.noteSessionOpened(rec.user) // #847: remote session also voids a pending confirm
	s.updateMetrics()
}

// applyRemoteSessionClose drops a remote session copy on SESSION-CLOSE gossip
// (#804). Only removes non-local records so a stray close can't evict a session
// this director actually owns.
func (s *Server) applyRemoteSessionClose(id string) {
	closedUser := ""
	s.sessRecMu.Lock()
	if rec, ok := s.sessById[id]; ok && rec.cl == nil {
		closedUser = rec.user
		delete(s.sessById, id)
		delete(s.sessByBE[rec.backend], id)
	}
	s.sessRecMu.Unlock()
	if closedUser != "" {
		s.noteSessionClosed(closedUser) // #847: gossiped close also arms the confirm
	}
	s.updateMetrics()
}
