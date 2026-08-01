// Package director — Membership implements the self-organizing director
// ring (#750, phase 1): members ordered by (ip, port), each node dialing
// only its right neighbor, joined via an HMAC-authenticated JOIN against a
// seed. Routing truth stays the deterministic ring hash (pkg/cluster/ring) —
// membership exists to redundantly share routing state between neighbors,
// not to elect or vote on anything.
//
// Degradation ladder (must hold at every member count):
//
//	N=1: no neighbors, no peer connections — today's single-replica mode.
//	N=2: left == right; exactly ONE connection serves both directions —
//	     only the lexicographically lower (ip,port) member dials.
//	N=3+: every member dials its right neighbor; N distinct directed edges.
//
// See the internal docs for the wire format.
package director

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Member identifies one director replica by its ring-protocol address.
// Ordering is (IP, Port) lexicographic — deterministic on every node, no
// separate node id: a restarted pod gets a new IP in k8s and simply rejoins
// as a fresh member (no identity to reconcile).
type Member struct {
	IP   string
	Port int
}

func (m Member) String() string { return net.JoinHostPort(m.IP, strconv.Itoa(m.Port)) }

func (m Member) isZero() bool { return m.IP == "" && m.Port == 0 }

func (m Member) equal(o Member) bool { return m.IP == o.IP && m.Port == o.Port }

// less orders members deterministically: IP first (by parsed numeric
// octets, NOT string comparison — "10.0.0.17" < "10.0.0.6" as strings,
// backwards from the actual numeric address, #754), then port. Every node
// computes the exact same ordering from the exact same member set, with
// zero coordination — that determinism is what makes the ring computable
// locally instead of needing a vote. Falls back to string comparison for
// an unparseable IP (should never happen in practice) so ordering stays
// total and deterministic either way.
func (m Member) less(o Member) bool {
	a, b := net.ParseIP(m.IP), net.ParseIP(o.IP)
	if a == nil || b == nil {
		if m.IP != o.IP {
			return m.IP < o.IP
		}
		return m.Port < o.Port
	}
	if c := bytes.Compare(a.To16(), b.To16()); c != 0 {
		return c < 0
	}
	return m.Port < o.Port
}

// Membership manages ring topology: the sorted member list, the single
// outgoing connection to this node's right neighbor, and (origin, seq)
// deduplicated event forwarding around the ring.
type Membership struct {
	srv        *Server
	self       Member
	secret     []byte // HMAC ring-join secret; empty = JOIN always rejected
	tlsCfg     *tls.Config
	minMembers int
	// joinAllowedNets restricts source CIDRs for DIRECTOR-JOIN (#773); nil/empty
	// = allow all. Checked before the HMAC challenge.
	joinAllowedNets []*net.IPNet
	// antiEntropyInterval paces antiEntropyLoop's periodic snapshot
	// re-broadcast; 0 = default (3s), negative = disabled. See the
	// Options field comment.
	antiEntropyInterval time.Duration
	// seedPollInterval paces joinLoop's periodic seed re-poll; 0 =
	// default (2s), negative = legacy one-shot join. See the Options
	// field comment.
	seedPollInterval time.Duration
	// seedPollIdleInterval is the eased poll cadence once the view has
	// reached min_members; 0 = default (2s = no effective backoff),
	// clamped up to seedPollInterval. See the Options field comment.
	seedPollIdleInterval time.Duration
	// resolveHost overrides seed hostname resolution in tests; nil =
	// stdlib resolver (see resolveSeed).
	resolveHost func(ctx context.Context, host string) ([]string, error)
	// tombstoneTTL bounds how long a tombstone outlives its death (#765);
	// 0 = default (10m), negative = never expire. See the removed field
	// comment for the expiry semantics.
	tombstoneTTL time.Duration
	// probeTimeout bounds one death-verification probe's whole exchange
	// (dial + handshake) when a neighbor connection is lost (#768); 0 =
	// default (2s). Unexported knob — tests shorten it; production stays
	// on the default.
	probeTimeout time.Duration

	mu      sync.RWMutex
	members []Member          // sorted, includes self once Start has run
	lastSeq map[string]uint64 // "ip:port" -> highest seq processed (dedup)
	seq     atomic.Uint64     // this node's own outgoing seq counter
	leaving atomic.Bool       // graceful shutdown in progress (#770): reject new JOINs
	// removed is the set of members known to be dead (#754), mapped to
	// when THIS node first learned of the death — a tombstone, not just a
	// transient absence from the current member list. Required because
	// DIRECTOR-LIST resync (mergeMembers) unions snapshots from
	// potentially-stale peers: without a tombstone, a peer who hasn't yet
	// learned of a death would silently resurrect the removed member on
	// every reconnect. addMember clears the tombstone for that (ip,port)
	// — a legitimate fresh authenticated JOIN (or a relayed DIRECTOR-ADD
	// vouching for one) is trusted to mean exactly that: this address is
	// alive again, whether it's a rejoin or the address was reassigned to
	// a genuinely new pod. Entries expire after tombstoneTTL (#765, lazy
	// deletion on read) so churn across many rollouts cannot grow the set
	// unboundedly; an incoming gossiped tombstone for an ALREADY-KNOWN
	// entry keeps the original local stamp (never refreshes), so a
	// tombstone ages out cluster-wide within roughly one TTL plus
	// propagation delay rather than being kept alive forever by the
	// periodic anti-entropy re-broadcast. Expiry is safe even if a stale
	// snapshot then resurrects the member: neighbor liveness monitoring
	// (#768 — dialRight on the right, onLeftConnLost on the left)
	// re-evicts an unreachable member within seconds regardless of
	// tombstone state.
	removed map[Member]time.Time

	rightMu     sync.Mutex
	rightTarget Member // zero Member{} = no active dial target
	// dialConn is the connection WE dial out (set by connectRight, managed
	// by reconcile's dial lifecycle) — reconcile tears it down and redials
	// when the computed right-neighbor target changes.
	//
	// ringConns tracks EVERY currently live ring connection — dialConn plus
	// every connection accepted from a neighbor that dialed us — for the
	// life of each connection, independent of dial/accept role. Broadcasting
	// ring events (broadcastRing) sends to all of ringConns except whichever
	// one the event just arrived on, matching the reference's director_update_send
	// (skip only the arrival connection; (origin, seq) dedup in
	// handleEnvelope is what actually stops the flood once it loops back to
	// its author — see the internal docs). Earlier this repo instead kept a
	// single "the" forward path (dialConn, falling back to a passiveConn set
	// only for the N=2 tie-break's passive member) — that role was decided
	// once, when a connection was accepted, and never revisited as topology
	// changed on that same still-open connection: a 3→2 shrink could leave a
	// connection accepted under N=3 rules silently unable to forward
	// anything at all once its node became the N=2 passive side (#754
	// follow-up). ringConns has no such role to get stale — connections are
	// registered/deregistered purely by their own lifetime.
	// backendHashMiss counts CONSECUTIVE anti-entropy ticks a ring connection's
	// backend-set hash has disagreed with ours (#846); backendSyncAt rate-limits
	// snapshot pulls per connection. Both guarded by rightMu, cleared when the
	// connection is deregistered.
	backendHashMiss map[net.Conn]int
	backendSyncAt   map[net.Conn]time.Time

	dialConn net.Conn
	// ringConns tracks every live ring connection (see the long comment above);
	// the value records who is on the other end and since when (#833) so admin
	// ring-status can name each edge and its uptime. Metadata only — broadcast
	// still just iterates the keys, and nothing on the wire changed.
	ringConns   map[net.Conn]ringConnMeta
	rightCancel context.CancelFunc

	ctx context.Context //nolint:containedctx // reconcile is triggered from accept-time handlers with no request-scoped ctx of their own; stored once at Start like Server's other background loops
}

// NewMembership creates a Membership for self, not yet started. secret
// authenticates incoming JOINs (empty = ring auth disabled, every JOIN
// rejected — #750 phase 2 adds dial-back + CIDR filtering on top of this
// HMAC core). tlsCfg wraps outgoing right-neighbor dials when non-nil.
// minMembers is an install-time warning threshold only; it never refuses
// service.
func NewMembership(srv *Server, self Member, secret []byte, tlsCfg *tls.Config, minMembers int, joinAllowedNets []*net.IPNet) *Membership {
	return &Membership{
		srv:             srv,
		self:            self,
		secret:          secret,
		tlsCfg:          tlsCfg,
		minMembers:      minMembers,
		joinAllowedNets: joinAllowedNets,
		lastSeq:         make(map[string]uint64),
		removed:         make(map[Member]time.Time),
		ringConns:       make(map[net.Conn]ringConnMeta),
		backendHashMiss: make(map[net.Conn]int),
		backendSyncAt:   make(map[net.Conn]time.Time),
	}
}

// ringConnMeta records who is on the other end of a live ring connection and
// since when (#833). Populated at register time on both the dial-out and the
// accept side so admin ring-status can name each edge and report its uptime;
// it is pure local bookkeeping — no wire change, and broadcast still only
// iterates ringConns' keys.
type ringConnMeta struct {
	peer  Member
	since time.Time
	role  string // "dial" = we dialed out to peer; "accept" = peer dialed us
}

// Start initializes the member list to [self] and, if seeds are given,
// begins trying to join the ring in the background. With no seeds (or
// until a join succeeds), the node runs as a valid N=1 ring — it never
// refuses service while waiting.
func (m *Membership) Start(ctx context.Context, seeds []string) {
	m.ctx = ctx
	m.mu.Lock()
	m.members = []Member{m.self}
	m.mu.Unlock()

	if m.minMembers > 1 && len(seeds) == 0 {
		slog.Warn("director: min_members configured above 1 but no seeds given — running as a permanent singleton ring (no state redundancy)",
			"min_members", m.minMembers)
	}
	// Anti-entropy runs regardless of seeds: a founder node has no seeds
	// but still accepts joins and holds ring connections that need the
	// periodic snapshot (#759).
	if m.antiEntropyInterval >= 0 {
		go m.antiEntropyLoop(ctx)
	}
	if len(seeds) == 0 {
		slog.Info("director: ring membership starting as singleton (no seeds configured)")
		return
	}
	go m.joinLoop(ctx, seeds)
}

// antiEntropyLoop periodically re-broadcasts this node's member+tombstone
// snapshot (the same DIRECTOR-LIST line every connection already exchanges
// once at connect time) over every live ring connection (#759). Receivers
// union it via mergeMembers, which is idempotent — so this is a bounded,
// no-coordination safety net: any membership split where at least one
// connection crosses the two views heals within one interval, instead of
// depending on every individual ADD/REMOVE broadcast having survived every
// concurrent-formation race. A split with NO crossing connection (fully
// disjoint subrings) is out of this loop's reach — that needs #750 phase
// 4's seed re-poll. Cost: one line per connection per interval, only sent
// when at least one connection exists.
func (m *Membership) antiEntropyLoop(ctx context.Context) {
	interval := m.antiEntropyInterval
	if interval == 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.broadcastRing(nil, fmt.Sprintf("DIRECTOR-LIST\t%s\t%s",
			formatMemberList(m.Members()), formatMemberList(m.removedList())))
		// Piggyback the backend-set hash on the same tick (#846) — a separate
		// line keeps the members plane and the backends plane cleanly apart.
		m.broadcastBackendHash()
	}
}

// Members returns a snapshot of the current ring membership, sorted.
func (m *Membership) Members() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Member, len(m.members))
	copy(out, m.members)
	return out
}

// ---- join (client side: dial a seed, authenticate, learn the ring) --------

// errJoinSelfDial marks a JOIN rejected because the load-balanced seed
// routed the dial back to this node itself (#759): with a ClusterIP seed
// in front of N pods this is an expected, instant outcome roughly 1/N of
// the time, not evidence the seed is unhealthy — joinLoop retries it on a
// short fixed interval instead of feeding it into the exponential backoff
// reserved for a genuinely unreachable seed. Live 3-pod evidence for why
// this distinction matters: treating self-dials as ordinary failures
// pushed one pod through 16s+ backoff windows per (expected!) rejection,
// stretching its convergence past 60s while its two peers took ~14s.
var errJoinSelfDial = errors.New("director/join: self-dial via load-balanced seed")

// joinLoop joins the ring via the seeds, then keeps polling a seed
// periodically forever (#759 Fix A) — it never returns while ctx lives
// (unless seed polling is explicitly disabled, which restores the old
// one-shot join). The periodic re-poll exists because every OTHER
// convergence mechanism (DIRECTOR-ADD/REMOVE broadcast, the anti-entropy
// snapshot) only travels over ring connections that already exist, and
// every mechanism that CREATES connections is a single right-neighbor
// dial computed from this node's own — possibly divergent — member view.
// Under concurrent formation those single dials are not guaranteed to
// form one connected graph (live-reproduced: two nodes closing a private
// 2-cycle while a third, better-informed node's knowledge never crossed
// into it), at which point no amount of flooding over existing
// connections can heal the split. The seed — one shared ClusterIP every
// pod can always reach — is the one guaranteed crossing point, so
// re-polling it bounds every partition's lifetime by the poll interval
// regardless of what the dial graph looks like.
//
// Pacing: seedPollInterval (default 2s) while this node's view holds
// FEWER than min_members; once it reaches the expected cluster size the
// cadence eases to seedPollIdleInterval. That idle interval defaults to
// the SAME 2s — i.e. constant polling by default — because a node cannot
// locally tell "converged" from "stable but holding a dead member": a
// freshly-respawned replacement pod that learned a since-dead member
// during the death-detection window sits at min_members-or-more with a
// stale entry, and a lazy idle cadence there leaves the dead member in
// place for a full idle interval (#765 — live-measured 40s+ with a 30s
// idle). Operators who want to trade steady-state polling for slower
// dead-member eviction on fresh joiners can raise seed_poll_idle_interval
// explicitly. Gating on min_members (not on own-view stability) is still
// deliberate: a partitioned node below the target size must never back
// off, since its view is perfectly stable yet wrong (#759). A self-dial
// rejection retries fast (see errJoinSelfDial); a genuinely unreachable
// seed walks the exponential backoff. Re-polls are cheap on the seed
// side too — handleJoin treats a known joiner as a read-only snapshot
// request (no DIRECTOR-ADD, no reconcile).
func (m *Membership) joinLoop(ctx context.Context, seeds []string) {
	pollInterval := m.seedPollInterval
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}
	oneShot := pollInterval < 0

	idleInterval := m.seedPollIdleInterval
	if idleInterval <= 0 {
		idleInterval = 2 * time.Second
	}
	if idleInterval < pollInterval {
		idleInterval = pollInterval
	}
	backoff := 2 * time.Second
	joined := false
	for {
		selfDial := false
		ok := false
		for _, addr := range seeds {
			if ctx.Err() != nil {
				return
			}
			polled, err := m.pollSeed(ctx, addr)
			if err == nil && polled {
				ok = true
				if !joined {
					joined = true
					slog.Info("director: joined ring", "seed", addr, "members", m.Members())
					m.reconcile()
				}
				break
			}
			if errors.Is(err, errJoinSelfDial) {
				selfDial = true
			}
			if err != nil {
				slog.Debug("director: ring join attempt failed", "seed", addr, "err", err)
			}
		}
		if ok && oneShot {
			return
		}

		var wait time.Duration
		switch {
		case ok:
			backoff = 2 * time.Second
			wait = pollInterval
			if m.minMembers > 1 && len(m.Members()) >= m.minMembers {
				wait = idleInterval
			}
		case selfDial:
			// The seed answered (with ourselves) — it is reachable and
			// serving; kube-proxy just needs another roll of the dice.
			wait = 500 * time.Millisecond
		default:
			wait = backoff
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// pollSeed polls one configured seed once, expanding a hostname seed into
// per-member candidates first (#759): a literal-IP seed is dialed as-is,
// but a hostname — in k8s the headless -director-ring Service, whose DNS
// answer is the complete list of ready pod IPs — is resolved explicitly
// and EVERY resulting address except this node's own is polled this
// cycle. Resolving ourselves matters twice over: it turns one poll into a
// deterministic sweep of every peer (convergence in one cycle, no routing
// luck), and it sidesteps Go's RFC 6724 destination ordering, which sorts
// the address sharing the longest prefix with the source first — i.e. the
// pod's OWN IP, which a naive sequential dial would then pick every
// single time. Returns polled=false only when every candidate was self
// (single-member DNS answer while alone), with err carrying the self-dial
// classification when applicable.
func (m *Membership) pollSeed(ctx context.Context, addr string) (polled bool, err error) {
	candidates := m.expandSeed(ctx, addr)
	if len(candidates) == 0 {
		return false, fmt.Errorf("%w: seed %s resolves only to this node", errJoinSelfDial, addr)
	}
	var lastErr error
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return polled, lastErr
		}
		if vErr := m.joinVia(ctx, cand); vErr != nil {
			lastErr = vErr
			continue
		}
		polled = true
	}
	if polled {
		return true, nil
	}
	return false, lastErr
}

// expandSeed turns a configured seed address into the list of candidate
// addresses to poll this cycle, excluding this node itself. Literal-IP
// seeds and hostnames that fail to resolve pass through unchanged (the
// dial itself will surface the error); a resolvable hostname fans out to
// every A/AAAA answer with the seed's port.
func (m *Membership) expandSeed(ctx context.Context, addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{addr}
	}
	if net.ParseIP(host) != nil {
		if (Member{IP: host, Port: atoiOr(port, 0)}).equal(m.self) {
			return nil
		}
		return []string{addr}
	}
	ips, err := m.resolveSeed(ctx, host)
	if err != nil || len(ips) == 0 {
		return []string{addr}
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		// The stdlib resolver returns bare IPs (the seed's port applies
		// to all of them — every director pod listens on the same port);
		// a test resolver may return full host:port entries instead,
		// since in-process test members share one IP and differ by port.
		if h, p, sErr := net.SplitHostPort(ip); sErr == nil {
			if (Member{IP: h, Port: atoiOr(p, 0)}).equal(m.self) {
				continue
			}
			out = append(out, ip)
			continue
		}
		if (Member{IP: ip, Port: atoiOr(port, 0)}).equal(m.self) {
			continue
		}
		out = append(out, net.JoinHostPort(ip, port))
	}
	return out
}

// resolveSeed resolves a seed hostname to IPs — swappable in tests (the
// production default is the stdlib resolver).
func (m *Membership) resolveSeed(ctx context.Context, host string) ([]string, error) {
	if m.resolveHost != nil {
		return m.resolveHost(ctx, host)
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("director/join: resolve seed: %w", err)
	}
	return ips, nil
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// joinVia dials addr, consumes its normal server handshake, then performs
// the DIRECTOR-JOIN / JOIN-CHALLENGE / JOIN-PROOF / JOIN-OK+DIRECTOR-LIST
// exchange. On success it merges the returned member list (plus self) into
// m.members and returns nil. This connection is bootstrap-only — it is
// closed after DIRECTOR-LIST regardless of outcome; the actual ring data
// connection is a separate dial to the computed right neighbor.
func (m *Membership) joinVia(ctx context.Context, addr string) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if m.tlsCfg != nil {
		// tls.Dialer (not DialWithDialer) so a ctx deadline — the liveness
		// probes run with a short one (#765) — bounds the TLS dial too.
		td := &tls.Dialer{NetDialer: dialer, Config: m.tlsCfg}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("director/join: dial: %w", err)
	}
	defer conn.Close()
	// A ctx deadline must bound the whole exchange, not just the dial: a
	// half-dead peer that accepts but never answers would otherwise hang
	// the handshake reads indefinitely (#765 — liveness probes must fail
	// fast, and a failed probe IS the signal).
	if dl, hasDeadline := ctx.Deadline(); hasDeadline {
		_ = conn.SetDeadline(dl)
	}

	rd := bufio.NewReaderSize(conn, 4096)
	if err := consumeServerHandshake(rd); err != nil {
		return fmt.Errorf("director/join: handshake: %w", err)
	}

	if _, err := fmt.Fprintf(conn, "DIRECTOR-JOIN\t%s\t%d\n", m.self.IP, m.self.Port); err != nil {
		return fmt.Errorf("director/join: send JOIN: %w", err)
	}

	line, err := readBoundedLine(rd)
	if err != nil {
		return fmt.Errorf("director/join: read challenge: %w", err)
	}
	fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
	if fields[0] == "JOIN-FAIL" {
		return joinFailError(fields[1:])
	}
	if fields[0] != "JOIN-CHALLENGE" || len(fields) < 2 {
		return fmt.Errorf("director/join: unexpected reply: %q", line)
	}
	nonce := fields[1]

	proof := hex.EncodeToString(joinHMAC(m.secret, nonce, m.self))
	if _, err := fmt.Fprintf(conn, "JOIN-PROOF\t%s\n", proof); err != nil {
		return fmt.Errorf("director/join: send proof: %w", err)
	}

	line, err = readBoundedLine(rd)
	if err != nil {
		return fmt.Errorf("director/join: read result: %w", err)
	}
	fields = strings.Split(strings.TrimRight(line, "\n"), "\t")
	if fields[0] == "JOIN-FAIL" {
		return joinFailError(fields[1:])
	}
	if fields[0] != "JOIN-OK" {
		return fmt.Errorf("director/join: unexpected reply: %q", line)
	}

	line, err = readBoundedLine(rd)
	if err != nil {
		return fmt.Errorf("director/join: read member list: %w", err)
	}
	fields = strings.Split(strings.TrimRight(line, "\n"), "\t")
	if fields[0] != "DIRECTOR-LIST" || len(fields) < 2 {
		return fmt.Errorf("director/join: expected DIRECTOR-LIST, got %q", line)
	}
	members := parseMemberList(fields[1])
	var removed []Member
	if len(fields) >= 3 {
		removed = parseMemberList(fields[2])
	}

	// After DIRECTOR-LIST the acceptor streams a userDir snapshot (#772) —
	// zero or more USER lines — terminated by DONE. Merge each so a
	// freshly-joined/restarted director inherits current sticky routing
	// state immediately instead of starting empty.
	for {
		line, err := readBoundedLine(rd)
		if err != nil {
			return fmt.Errorf("director/join: read snapshot: %w", err)
		}
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if f[0] == "DONE" {
			break
		}
		if f[0] == "USER" {
			m.applyUserLine(f)
		}
	}

	// Union, never replace (#759 Fix A): on a periodic re-poll the seed
	// answering may itself be the stale one — a replace would regress this
	// node's view to the seed's. mergeMembers adds self, applies both
	// tombstone sets, and reconciles only when something actually changed,
	// which also makes the steady-state poll a complete no-op locally.
	m.mergeMembers(members, removed)
	return nil
}

// dialBackTimeout bounds the whole dial-back exchange (#773). A single attempt,
// no retry: a genuine director on a flat pod network answers well within this;
// anything slower is treated as "did not prove it controls the address". Kept
// short so a spoofed/unreachable ME cannot stall a join handler.
const dialBackTimeout = 3 * time.Second

// verifyDialBack independently dials the address the joiner CLAIMED (its ME
// ip:port) and confirms a live director answers there (#773) — proof the joiner
// really controls that address rather than feeding a forged ME. It is a plain
// client connection: it reads the server handshake (VERSION..DONE) and closes,
// sending NO DIRECTOR-JOIN / ME / PEER, so the acceptor side treats it as an
// anonymous probe and never mutates its membership or triggers a reverse join —
// which is what keeps simultaneous N-pod formation from cascading into a storm
// of cross dial-backs (every side's probe is independent, short, and inert).
//
// Must only be called after a valid HMAC proof — see the invariant note in
// handleJoin.
func (m *Membership) verifyDialBack(joiner Member) error {
	ctx, cancel := context.WithTimeout(m.ctx, dialBackTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: dialBackTimeout}
	var conn net.Conn
	var err error
	if m.tlsCfg != nil {
		td := &tls.Dialer{NetDialer: dialer, Config: m.tlsCfg}
		conn, err = td.DialContext(ctx, "tcp", joiner.String())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", joiner.String())
	}
	if err != nil {
		return fmt.Errorf("director/dialback: dial %s: %w", joiner, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	rd := bufio.NewReaderSize(conn, 4096)
	if err := consumeServerHandshake(rd); err != nil {
		return fmt.Errorf("director/dialback: handshake %s: %w", joiner, err)
	}
	return nil
}

// ipInNets reports whether addr's IP falls inside any of nets. A nil/unparseable
// address or IP is never allowed (caller gates on len(nets) > 0 for allow-all).
func ipInNets(addr net.Addr, nets []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// consumeServerHandshake reads and discards VERSION..HOST-HAND-*..DONE. The
// join connection doesn't need the backend list — the real ring/PEER
// connection (dialRight) already learns it the same way client connections
// always have.
func consumeServerHandshake(rd *bufio.Reader) error {
	for {
		line, err := readBoundedLine(rd)
		if err != nil {
			return err
		}
		if strings.TrimRight(line, "\n") == "DONE" {
			return nil
		}
	}
}

// joinFailError maps a JOIN-FAIL's reason fields to the error joinLoop
// keys its retry policy on: a reason starting with "self-dial" (the stable
// prefix handleJoin sends, kept stable as wire contract) becomes
// errJoinSelfDial; anything else is an ordinary rejection.
func joinFailError(reason []string) error {
	msg := strings.Join(reason, " ")
	if strings.HasPrefix(msg, "self-dial") {
		return fmt.Errorf("%w: %s", errJoinSelfDial, msg)
	}
	return fmt.Errorf("director/join: rejected: %s", msg)
}

func joinHMAC(secret []byte, nonce string, joiner Member) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(nonce))
	mac.Write([]byte("\t"))
	mac.Write([]byte(joiner.IP))
	mac.Write([]byte("\t"))
	mac.Write([]byte(strconv.Itoa(joiner.Port)))
	return mac.Sum(nil)
}

func parseMemberList(csv string) []Member {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]Member, 0, len(parts))
	for _, p := range parts {
		host, portStr, err := net.SplitHostPort(p)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		out = append(out, Member{IP: host, Port: port})
	}
	return out
}

func formatMemberList(members []Member) string {
	parts := make([]string, len(members))
	for i, mem := range members {
		parts[i] = mem.String()
	}
	return strings.Join(parts, ",")
}

// setMembers replaces the member set with a deduplicated, sorted copy,
// excluding anything in the tombstone set (m.removed) except self (self is
// never tombstoned against itself). Caller decides whether the result
// warrants a reconcile.
func (m *Membership) setMembers(members []Member) {
	m.mu.Lock()
	seen := make(map[Member]bool, len(members))
	uniq := make([]Member, 0, len(members))
	for _, mem := range members {
		if seen[mem] {
			continue
		}
		if m.tombstonedLocked(mem) && !mem.equal(m.self) {
			continue
		}
		seen[mem] = true
		uniq = append(uniq, mem)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].less(uniq[j]) })
	m.members = uniq
	m.mu.Unlock()
}

// tombstonedLocked reports whether mem currently counts as tombstoned,
// lazily deleting an entry whose TTL has elapsed (#765). Caller must hold
// m.mu (write). TTL <= negative never expires; 0 uses the default.
func (m *Membership) tombstonedLocked(mem Member) bool {
	stamp, dead := m.removed[mem]
	if !dead {
		return false
	}
	ttl := m.tombstoneTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if ttl > 0 && time.Since(stamp) > ttl {
		delete(m.removed, mem)
		return false
	}
	return true
}

// mergeMembers unions incoming members and tombstones with this node's own
// (self always present in the result, never tombstoned) and, if that
// changes anything, applies it and reconciles. Used for the DIRECTOR-LIST
// resync exchanged on every ring connection — a plain union rather than a
// replace, so a snapshot that's simply stale (missing a member or a
// removal we already know about from elsewhere) can't regress us. The
// tombstone side of the union is what makes this safe against resurrecting
// a member some OTHER path already declared dead (#754) — without it, any
// peer whose own view hadn't caught up yet would silently undo a correct
// removal on every reconnect.
func (m *Membership) mergeMembers(incomingMembers, incomingRemoved []Member) {
	m.mu.Lock()
	for _, mem := range incomingRemoved {
		if mem.equal(m.self) {
			continue
		}
		// Keep the original local stamp for an already-known tombstone:
		// re-stamping on every gossiped copy would let the periodic
		// anti-entropy re-broadcast refresh the TTL forever, so nothing
		// would ever age out (#765).
		if _, known := m.removed[mem]; !known {
			m.removed[mem] = time.Now()
		}
	}
	current := append([]Member{}, m.members...)
	before := len(m.members)
	m.mu.Unlock()

	all := append(append([]Member{}, current...), incomingMembers...)
	all = append(all, m.self)
	m.setMembers(all)

	m.mu.RLock()
	after := len(m.members)
	m.mu.RUnlock()
	if after != before {
		m.reconcile()
	}
}

// ---- userDir state exchange (#772) ----------------------------------------

// sendUserSnapshot streams this director's userDir as USER lines over conn,
// sent right after DIRECTOR-LIST on every ring (re)connect so a fresh or
// restarted director inherits current sticky routing state immediately
// (#772 — the reference's U-line handshake). Wire form, one per entry:
//
//	USER\t<hash>\t<host>\t<assign_seq>\t<assign_by>\t<weak>\n
//
// O(users), sent only on connect; the ongoing per-LOOKUP propagation is a
// separate concern (#772 PR-2).
func (m *Membership) sendUserSnapshot(conn net.Conn) {
	if m.srv == nil {
		return
	}
	for _, e := range m.srv.userDir.Snapshot() {
		weak := "0"
		if e.Weak {
			weak = "1"
		}
		_, _ = fmt.Fprintf(conn, "USER\t%d\t%s\t%d\t%s\t%s\n", e.Hash, e.Host, e.AssignSeq, e.AssignBy, weak)
	}
}

// applyUserLine merges one received USER snapshot line into the local
// userDir under the (AssignSeq, AssignBy) total order (#772). Malformed
// lines are ignored.
func (m *Membership) applyUserLine(fields []string) {
	if m.srv == nil || len(fields) < 6 {
		return
	}
	hash, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return
	}
	seq, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return
	}
	if old := m.srv.userDir.MergeByHash(uint32(hash), fields[2], fields[5] == "1", seq, fields[4]); old != "" {
		m.srv.kickStaleSessions(uint32(hash), old)
	}
}

// removedList returns a snapshot of the live (non-expired) tombstone set,
// for exchange in a DIRECTOR-LIST resync — expired entries are dropped
// here too so they stop being gossiped (#765).
func (m *Membership) removedList() []Member {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Member, 0, len(m.removed))
	for mem := range m.removed {
		if m.tombstonedLocked(mem) {
			out = append(out, mem)
		}
	}
	return out
}

// addMember admits mem as live: adds it to the member set (no-op if
// already present) and clears any tombstone for it — a fresh authenticated
// JOIN (or a relayed DIRECTOR-ADD vouching for one) is trusted to mean
// this address is alive now, whether that's a genuine rejoin or the
// address was reassigned to a new pod (#754).
func (m *Membership) addMember(mem Member) {
	m.mu.Lock()
	delete(m.removed, mem)
	for _, existing := range m.members {
		if existing.equal(mem) {
			m.mu.Unlock()
			return
		}
	}
	m.members = append(m.members, mem)
	sort.Slice(m.members, func(i, j int) bool { return m.members[i].less(m.members[j]) })
	m.mu.Unlock()
}

// removeMember evicts mem from the live member set and tombstones it
// (#754) so a later DIRECTOR-LIST resync from a peer that hasn't yet
// learned of the removal can't silently resurrect it.
func (m *Membership) removeMember(mem Member) {
	m.mu.Lock()
	for i, existing := range m.members {
		if existing.equal(mem) {
			m.members = append(m.members[:i], m.members[i+1:]...)
			break
		}
	}
	if !mem.equal(m.self) {
		m.removed[mem] = time.Now()
	}
	m.mu.Unlock()
}

// ---- join (server side: accept a JOIN, authenticate, admit) ---------------

// handleJoin runs the HMAC challenge/response for a DIRECTOR-JOIN request
// already read as fields (DIRECTOR-JOIN, ip, port) and owns the rest of the
// connection's lifetime — it always closes conn before returning. Called
// from handleConn in place of the normal client handshake loop.
func (m *Membership) handleJoin(conn net.Conn, fields []string) {
	if len(fields) < 3 {
		return
	}
	if m.leaving.Load() {
		// Graceful shutdown in progress (#770): a fresh joiner must not
		// learn this dying member (the #765 phantom-injection source), and
		// a re-poll must not re-add us to a peer that already got our
		// DIRECTOR-REMOVE.
		_ = writeLine(conn, "JOIN-FAIL\tshutting down")
		return
	}
	joiner := Member{IP: fields[1]}
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	joiner.Port = port

	// CIDR allow-list (#773) — the cheapest filter, run first and before the
	// HMAC challenge: keep the ring-join surface off untrusted networks without
	// spending a nonce/HMAC round on packets that will never be admitted. The
	// source of truth is the ACTUAL peer address (conn.RemoteAddr), never the
	// claimed ME fields, so a spoofed ME cannot dodge the filter. Empty list =
	// allow all (unchanged behaviour).
	if len(m.joinAllowedNets) > 0 && !ipInNets(conn.RemoteAddr(), m.joinAllowedNets) {
		_ = writeLine(conn, "JOIN-FAIL\tnot allowed from this network")
		slog.Warn("director: JOIN rejected, source not in join_allowed_nets", "remote", conn.RemoteAddr())
		joinRejected.Inc()
		return
	}

	if joiner.equal(m.self) {
		// A load-balanced ClusterIP seed can route a pod's own JOIN dial
		// back to itself (#759): kube-proxy has no reason to avoid it.
		// Accepting it looks like a normal join (the "joiner" is already
		// self, so addMember is a no-op) but never actually discovers any
		// other member — and joinLoop stops retrying the seed the moment
		// it believes it has joined, leaving the pod stuck as a permanent,
		// silent N=1. Reject explicitly instead, so joinVia returns an
		// error and joinLoop's existing generic retry keeps dialing the
		// seed until kube-proxy happens to route it elsewhere.
		_ = writeLine(conn, "JOIN-FAIL\tself-dial via load-balanced seed, retry")
		slog.Debug("director: JOIN rejected, self-dial via load-balanced seed", "self", m.self)
		return
	}

	if len(m.secret) == 0 {
		_ = writeLine(conn, "JOIN-FAIL\tring auth not configured")
		slog.Warn("director: JOIN rejected, ring auth not configured", "joiner", joiner)
		joinRejected.Inc()
		return
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		_ = writeLine(conn, "JOIN-FAIL\tinternal error")
		return
	}
	nonceHex := hex.EncodeToString(nonce)
	if err := writeLine(conn, "JOIN-CHALLENGE\t"+nonceHex); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReaderSize(conn, 1024)
	line, err := readBoundedLine(rd)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	proofFields := strings.Split(strings.TrimRight(line, "\n"), "\t")
	if len(proofFields) < 2 || proofFields[0] != "JOIN-PROOF" {
		_ = writeLine(conn, "JOIN-FAIL\texpected JOIN-PROOF")
		return
	}
	got, err := hex.DecodeString(proofFields[1])
	if err != nil {
		_ = writeLine(conn, "JOIN-FAIL\tmalformed proof")
		joinRejected.Inc()
		return
	}
	want := joinHMAC(m.secret, nonceHex, joiner)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		_ = writeLine(conn, "JOIN-FAIL\tinvalid proof")
		slog.Warn("director: JOIN rejected, invalid HMAC proof", "joiner", joiner)
		joinRejected.Inc()
		return
	}

	// Dial-back verification (#773) — MUST run only AFTER a valid HMAC proof,
	// and this ordering is an anti-amplification invariant, not an optimisation:
	// the dial-back makes THIS director connect to the joiner's CLAIMED ME
	// address. Were it reachable before the proof, any host in join_allowed_nets
	// could send a DIRECTOR-JOIN carrying a victim's ME and turn the ring into a
	// reflector that dials arbitrary addresses on command. Gating it behind the
	// proof means only a genuine ring member (one that knows the secret) can
	// ever cause a dial-back, and it is aimed at that member's own claimed
	// address. The proof establishes membership; the dial-back then establishes
	// that the joiner truly controls the address it claims (not a spoofed ME) by
	// confirming a live director answers there.
	if err := m.verifyDialBack(joiner); err != nil {
		_ = writeLine(conn, "JOIN-FAIL\tdial-back verification failed")
		slog.Warn("director: JOIN rejected, dial-back verification failed", "joiner", joiner, "err", err)
		joinRejected.Inc()
		return
	}

	// Snapshot the member list BEFORE adding the joiner — DIRECTOR-LIST
	// tells the joiner who else is here; it adds itself locally. A joiner
	// we already know is a periodic seed re-poll (#759 Fix A), not a new
	// member: it still gets the full list+tombstone snapshot (that
	// exchange IS the poll's purpose — the seed is the one guaranteed
	// crossing point every pod can always reach), but no DIRECTOR-ADD is
	// broadcast and no reconcile runs, so a 2s poll cadence across the
	// fleet costs nothing beyond this one reply.
	existing := m.Members()
	already := false
	for _, mem := range existing {
		if mem.equal(joiner) {
			already = true
			break
		}
	}
	m.addMember(joiner)

	if err := writeLine(conn, "JOIN-OK"); err != nil {
		return
	}
	if err := writeLine(conn, "DIRECTOR-LIST\t"+formatMemberList(existing)+"\t"+formatMemberList(m.removedList())); err != nil {
		return
	}
	m.sendUserSnapshot(conn) // #772: userDir state, between DIRECTOR-LIST and DONE
	_ = writeLine(conn, "DONE")

	if already {
		slog.Debug("director: ring re-join poll served", "joiner", joiner)
		return
	}

	slog.Info("director: ring join accepted", "joiner", joiner, "members", len(existing)+1)
	joinAccepted.Inc()

	// Broadcast the join over the OLD topology's connections first, THEN
	// rebuild edges (#759 problem 2). reconcile() may tear down this
	// node's dial-out to its previous right neighbor (the joiner sorts
	// between them), and under concurrent formation the replacement dial
	// races the broadcast — live 3-pod evidence: an acceptor that learned
	// of a third member never told its still-directly-connected earlier
	// neighbor, leaving it permanently stuck at 2 members. Every current
	// member is reachable over the pre-reconcile connection set by
	// definition, so originate-first cannot lose the event; the reverse
	// order can. (The historical reason for reconcile-first — originate
	// used to send only via dialConn, nil right after a death — died with
	// broadcastRing, which uses every live connection.)
	m.originate("DIRECTOR-ADD", fmt.Sprintf("%s\t%d", joiner.IP, joiner.Port))
	m.reconcile()
}

func writeLine(conn net.Conn, s string) error {
	_, err := fmt.Fprintf(conn, "%s\n", s)
	return err
}

// readBoundedLine reads one LF-terminated line, hard-capped at rd's buffer size.
// ReadSlice — unlike ReadString, which keeps appending without bound
// (#703) — fails with ErrBufferFull once the buffer fills with no
// newline, so a misbehaving or malicious peer streaming bytes without LF
// costs at most one buffer per connection, not unbounded memory; callers
// treat the error like any other read failure and drop the connection.
// The returned line keeps its trailing \n, matching ReadString's
// contract, so call sites trim exactly as before.
func readBoundedLine(rd *bufio.Reader) (string, error) {
	line, err := rd.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("director: line exceeds %d bytes", rd.Size())
		}
		return "", err
	}
	return string(line), nil
}

// ---- ring topology: neighbor computation + the single right-hand dial -----

// rightNeighbor returns the member self should dial as its right neighbor,
// per the degradation ladder: N=1 → none; N=2 → only the lexicographically
// lower member dials (so exactly one connection ever exists between the
// pair, serving both directions); N>=3 → the next member after self in
// sorted order, wrapping around.
func (m *Membership) rightNeighbor() (Member, bool) {
	members := m.Members()
	switch len(members) {
	case 0, 1:
		return Member{}, false
	case 2:
		a, b := members[0], members[1]
		if !m.self.equal(a) {
			// We're the higher-sorted member of the pair — we only accept.
			return Member{}, false
		}
		return b, true
	default:
		idx := -1
		for i, mem := range members {
			if mem.equal(m.self) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return Member{}, false // self not in the list yet
		}
		return members[(idx+1)%len(members)], true
	}
}

// rightNeighborOf returns who `of` should be dialing, using the same rule
// as rightNeighbor but for an arbitrary member — used by the accept side to
// decide whether an incoming dialer picked the correct target (#750's
// CONNECT-redirect trick).
func (m *Membership) rightNeighborOf(of Member) (Member, bool) {
	members := m.Members()
	switch len(members) {
	case 0, 1:
		return Member{}, false
	case 2:
		a, b := members[0], members[1]
		if !of.equal(a) {
			return Member{}, false
		}
		return b, true
	default:
		idx := -1
		for i, mem := range members {
			if mem.equal(of) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return Member{}, false
		}
		return members[(idx+1)%len(members)], true
	}
}

// leftNeighbor returns the member whose right-neighbor dial should be
// arriving at this node — the predecessor in sorted order, wrapping — i.e.
// the reference's dir->left (#768). N=1 has none. N=2: only the
// lexicographically lower member dials, so only the HIGHER member has a
// left (the lower one; the lower member's own guard is its dial-out,
// handled by dialRight).
func (m *Membership) leftNeighbor() (Member, bool) {
	members := m.Members()
	switch len(members) {
	case 0, 1:
		return Member{}, false
	case 2:
		a, b := members[0], members[1]
		if m.self.equal(b) {
			return a, true
		}
		return Member{}, false
	default:
		idx := -1
		for i, mem := range members {
			if mem.equal(m.self) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return Member{}, false
		}
		return members[(idx-1+len(members))%len(members)], true
	}
}

// reconcile recomputes this node's right-neighbor DIAL target and adjusts
// it to match: tears down a stale dial and starts a new one when the
// target changed, including down to "no target" at N=1 or the N=2 passive
// case. Deliberately never touches ringConns — an accepted connection's
// membership there is tied to its own lifetime, not to this node's current
// dial target; see the field comment.
func (m *Membership) reconcile() {
	target, ok := m.rightNeighbor()

	m.rightMu.Lock()
	defer m.rightMu.Unlock()

	if ok && m.rightTarget.equal(target) {
		return // unchanged
	}

	if m.rightCancel != nil {
		m.rightCancel()
		m.rightCancel = nil
	}
	if m.dialConn != nil {
		// QUIT before the deliberate close (#768, reference parity —
		// director-connection.c sends QUIT\t<reason> on every intentional
		// disconnect): the peer we're abandoning is usually OUR old right
		// neighbor, i.e. we are ITS left — without this line our close
		// looks identical to a silent death and forces it through the
		// verification-probe path for what is just a re-target.
		_ = m.dialConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = fmt.Fprintf(m.dialConn, "QUIT\tretargeting right neighbor\n")
		_ = m.dialConn.Close()
		m.dialConn = nil
	}
	m.rightTarget = Member{}

	if !ok || m.ctx == nil || m.ctx.Err() != nil {
		return
	}

	dialCtx, cancel := context.WithCancel(m.ctx)
	m.rightTarget = target
	m.rightCancel = cancel
	go m.dialRight(dialCtx, target)
}

// dialRight maintains the persistent connection to target: the standard
// VERSION/ME/PEER/DONE ring handshake (unchanged from #700), reading the
// HOST snapshot the same way client connections always have, then
// forwarding ring-envelope pushes until the connection fails — at which
// point target is declared dead: removed locally, announced via
// DIRECTOR-REMOVE, and reconcile() picks the next right neighbor
// ("skip-dead"). A CONNECT redirect (stale target after a membership
// change mid-dial) retries immediately against the corrected address
// instead of going through the dead-declaration path.
func (m *Membership) dialRight(ctx context.Context, target Member) {
	const maxAttempts = 3
	// current tracks who we're ACTUALLY trying to reach right now — starts
	// as target, but a CONNECT redirect can point us elsewhere (#754).
	// Every decision keyed on "who failed" (the still-alive short-circuit,
	// and the final death declaration) must use current, never the
	// original target — conflating the two previously meant that
	// following a redirect toward an already-dead member, then exhausting
	// retries against IT, would wrongly declare the ORIGINAL (perfectly
	// alive) target dead instead.
	current := target
	addr := current.String()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		redirect, err := m.connectRight(ctx, addr)
		if redirect != "" {
			addr = redirect
			if host, portStr, splitErr := net.SplitHostPort(redirect); splitErr == nil {
				if port, convErr := strconv.Atoi(portStr); convErr == nil {
					current = Member{IP: host, Port: port}
				}
			}
			attempt = 0 // a redirect is not a failed attempt
			continue
		}
		if err == nil {
			// connectRight only returns nil after ctx was cancelled
			// (clean shutdown, e.g. reconcile picked a new target).
			return
		}
		slog.Debug("director: ring dial attempt failed", "self", m.self, "target", addr, "attempt", attempt, "err", err)
		// Someone else already reported (and propagated) this member's
		// death — most likely via DIRECTOR-LIST tombstone resync (#754).
		// Stop retrying a corpse instead of burning the remaining attempts.
		still := false
		for _, mem := range m.Members() {
			if mem.equal(current) {
				still = true
				break
			}
		}
		if !still {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}
	slog.Warn("director: ring neighbor unreachable, declaring dead", "self", m.self, "target", current)
	m.removeMember(current)
	// Announce over the surviving pre-reconcile connections first, then
	// rebuild edges — same broadcast-before-reconcile rule as handleJoin
	// (#759 problem 2): reconcile() may tear down a live connection the
	// announcement still needs, while the announcement can never need a
	// connection reconcile() has yet to create (the dead dial is already
	// gone, and any new right neighbor resyncs via DIRECTOR-LIST on
	// connect anyway).
	m.originate("DIRECTOR-REMOVE", fmt.Sprintf("%s\t%d", current.IP, current.Port))
	m.reconcile()
}

// connectRight performs one dial+handshake+read-loop against addr. Returns
// a non-empty redirect address if the acceptor sent CONNECT (caller should
// retry immediately against it); otherwise returns nil error only on clean
// ctx cancellation, and a non-nil error for any dial/handshake/read failure
// (caller treats that as one failed attempt, per dialRight's retry policy).
func (m *Membership) connectRight(ctx context.Context, addr string) (redirect string, err error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if m.tlsCfg != nil {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, m.tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	rd := bufio.NewReaderSize(conn, 4096)
	inHandshake := false
	for {
		line, rErr := readBoundedLine(rd)
		if rErr != nil {
			return "", fmt.Errorf("handshake read: %w", rErr)
		}
		line = strings.TrimRight(line, "\n")
		if line == "DONE" {
			break
		}
		switch {
		case line == "HOST-HAND-START":
			inHandshake = true
		case line == "HOST-HAND-END":
			inHandshake = false
		case inHandshake && strings.HasPrefix(line, "HOST\t"):
			applyHandshakeHost(m.srv, line)
		}
	}

	ts := time.Now().Unix()
	for _, s := range []string{
		fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer),
		fmt.Sprintf("ME\t%s\t%d\t%d", m.self.IP, m.self.Port, ts),
		// MEMBERS, sent before PEER (#754): the acceptor's CONNECT-redirect
		// decision (triggered by the PEER line, in handleConn) uses its
		// OWN membership view — without this, a still-3-member acceptor
		// can redirect the dialer BACK toward a member the dialer already
		// knows is dead (it's dialing elsewhere precisely because it
		// detected that death), and the redirect path never reaches the
		// DIRECTOR-LIST resync that would otherwise fix the acceptor's
		// stale view — merging the dialer's tombstones first closes that
		// gap regardless of which way the connection ends up being used.
		fmt.Sprintf("MEMBERS\t%s\t%s", formatMemberList(m.Members()), formatMemberList(m.removedList())),
		"PEER\t1",
		"DONE",
	} {
		if _, wErr := fmt.Fprintf(conn, "%s\n", s); wErr != nil {
			return "", fmt.Errorf("handshake send: %w", wErr)
		}
	}

	m.rightMu.Lock()
	m.dialConn = conn
	m.ringConns[conn] = ringConnMeta{peer: m.rightTarget, since: time.Now(), role: "dial"}
	m.rightMu.Unlock()
	defer func() {
		m.rightMu.Lock()
		if m.dialConn == conn {
			m.dialConn = nil
		}
		delete(m.ringConns, conn)
		delete(m.backendHashMiss, conn)
		delete(m.backendSyncAt, conn)
		m.rightMu.Unlock()
	}()

	slog.Info("director: ring right-neighbor connected", "target", addr)

	// Exchange a full membership snapshot right away (#750 phase 1 fix): a
	// DIRECTOR-ADD fired between a join being accepted and that acceptor's
	// own dial to its (possibly just-changed) right neighbor completing
	// would otherwise race broadcastRing's best-effort delivery and vanish
	// silently — this connection is brand new either way, so unconditional
	// resync on connect closes that window regardless of timing, without
	// needing the full user/backend state snapshot (#750 phase 3).
	if _, wErr := fmt.Fprintf(conn, "DIRECTOR-LIST\t%s\t%s\n", formatMemberList(m.Members()), formatMemberList(m.removedList())); wErr != nil {
		return "", fmt.Errorf("member snapshot send: %w", wErr)
	}
	m.sendUserSnapshot(conn) // #772: userDir snapshot on connect

	// Keepalive both ways (#768): we PING our right neighbor over this
	// dial, it PINGs us back over it too (plus over whatever it dials);
	// the read deadline turns a silently-hung peer into a read error,
	// which dialRight's caller treats as one failed attempt — the same
	// death path an outright refused dial takes.
	go m.ringPinger(ctx, conn)
	readWindow := m.ringPingInterval() + m.ringPingTimeout()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readWindow))
		line, rErr := readBoundedLine(rd)
		if rErr != nil {
			if ctx.Err() != nil {
				return "", nil
			}
			return "", fmt.Errorf("read: %w", rErr)
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		switch {
		case fields[0] == "PING":
			_, _ = fmt.Fprintf(conn, "PONG\n")
		case fields[0] == "PONG":
			// keepalive traffic — arrival already refreshed the deadline
		case fields[0] == "QUIT":
			// The peer is closing deliberately (#768) — e.g. shutting
			// down. Surface it as an ordinary attempt failure with the
			// announced reason; dialRight's retry/death policy applies
			// unchanged (a deliberately-exiting right neighbor SHOULD
			// eventually be declared dead by it, that's the point).
			reason := ""
			if len(fields) >= 2 {
				reason = fields[1]
			}
			return "", fmt.Errorf("peer quit: %s", reason)
		case fields[0] == "CONNECT" && len(fields) >= 3:
			return net.JoinHostPort(fields[1], fields[2]), nil
		case fields[0] == "DIRECTOR-LIST" && len(fields) >= 2:
			removed := ""
			if len(fields) >= 3 {
				removed = fields[2]
			}
			m.mergeMembers(parseMemberList(fields[1]), parseMemberList(removed))
		case fields[0] == "USER":
			m.applyUserLine(fields)
		case strings.HasPrefix(fields[0], "BACKEND-") && m.handleBackendSyncLine(conn, rd, fields):
			// #846 backend-set resync sub-protocol (per-connection request/response)
		default:
			m.handleRingLine(fields, conn)
		}
	}
}

// ---- accept side: PEER connection admission + CONNECT-redirect ------------

// checkRedirect reports whether an incoming ring (PEER) connection from
// dialer should be redirected: the dialer is a known member, but this node
// is not the right neighbor dialer's topology says it should be dialing.
// Returns the corrected target and true when a CONNECT should be sent.
// Deliberately lenient when dialer isn't a recognized member yet (a fresh
// joiner's DIRECTOR-ADD may not have propagated to us yet) — accept rather
// than reject, matching the no-quorum/AP model.
func (m *Membership) checkRedirect(dialer Member) (Member, bool) {
	known := false
	for _, mem := range m.Members() {
		if mem.equal(dialer) {
			known = true
			break
		}
	}
	if !known {
		return Member{}, false
	}
	want, ok := m.rightNeighborOf(dialer)
	if !ok || want.equal(m.self) {
		return Member{}, false
	}
	return want, true
}

// ---- ring event forwarding: (origin, seq) dedup ---------------------------

// originate sends kind+payload around the ring as a fresh event authored by
// this node: self is the origin, seq is this node's next counter value.
// Only reaches the ring — local login clients are notified separately by
// the caller (Server.originateRingEvent) with the plain, non-enveloped line
// they've always received.
func (m *Membership) originate(kind, payload string) {
	seq := m.seq.Add(1)
	key := fmt.Sprintf("%s:%d", m.self.IP, m.self.Port)
	m.mu.Lock()
	m.lastSeq[key] = seq
	m.mu.Unlock()
	m.broadcastRing(nil, fmt.Sprintf("%s\t%s\t%d\t%d\t%s", kind, m.self.IP, m.self.Port, seq, payload))
}

// Leave performs a graceful ring exit (#770), called on SIGTERM BEFORE the
// process's ctx is cancelled (while ring connections are still open). It
// (1) stops answering JOINs so no fresh joiner can learn this dying member
// — the #765 phantom-injection source; (2) originates DIRECTOR-REMOVE for
// itself so peers evict it immediately via the existing seq-dedup +
// tombstone path, with zero death-detection window and no verification
// probes fired for a planned exit; and (3) sends QUIT on every live ring
// connection so each neighbor classifies the imminent close as deliberate
// (benign), not a silent death. Unlike the reference — whose members are
// static config and simply reconnect around a stopped director — our
// members are ephemeral pods whose IPs never return, so announcing the
// exit is the correct adaptation. Hard kills (SIGKILL) still converge via
// the #768 neighbor-monitoring path; this only removes the latency on the
// planned path. Best-effort: the caller should allow a brief flush before
// cancelling ctx.
func (m *Membership) Leave() {
	m.leaving.Store(true)
	m.originate("DIRECTOR-REMOVE", fmt.Sprintf("%s\t%d", m.self.IP, m.self.Port))

	m.rightMu.Lock()
	conns := make([]net.Conn, 0, len(m.ringConns))
	for c := range m.ringConns {
		conns = append(conns, c)
	}
	m.rightMu.Unlock()
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = fmt.Fprintf(c, "QUIT\tshutting down\n")
		_ = c.SetWriteDeadline(time.Time{})
	}
	slog.Info("director: graceful ring leave", "self", m.self)
}

// handleRingLine dispatches one line read from a ring connection: every
// envelope command (DIRECTOR-ADD/REMOVE, RING-CHANGE, USER-MOVED,
// USER-KICKED) goes through the shared (origin, seq) dedup + apply +
// forward path. arrivalConn is the connection this line was just read from
// — handleEnvelope needs it to skip re-sending the event back the way it
// came. PING/PONG keepalive lines are handled directly in the two ring
// read loops (serveRingConn / connectRight) before reaching here (#768);
// #750 phase 4 may later piggyback anti-entropy metadata on them.
func (m *Membership) handleRingLine(fields []string, arrivalConn net.Conn) {
	switch fields[0] {
	case "DIRECTOR-ADD", "DIRECTOR-REMOVE", "RING-CHANGE", "USER-MOVED", "USER-KICKED", "USER-ASSIGN",
		"SESSION-OPEN", "SESSION-CLOSE", "BACKEND-UNREACHABLE", "USER-KILLING", "USER-KILL-DONE":
		m.handleEnvelope(fields, arrivalConn)
	}
}

// handleEnvelope implements the propagation rule that makes the ring
// self-limiting with zero coordination: if the event's origin is this node
// itself, the event has travelled all the way around and is absorbed
// (applied once already, at origination — never re-applied, never
// forwarded again). Otherwise, apply locally (unless already seen — a
// safety net against out-of-order duplicates) and broadcast to every ring
// connection except the one it just arrived on (broadcastRing) — matching
// the reference's director_update_send skip-arrival rule rather than a fixed
// "the" forward path. At N=2 the only ring connection IS the arrival
// connection, so broadcasting sends nowhere and the event simply stops
// there without needing to bounce back to origin for absorb to apply —
// origin-absorb only matters once N>=3 gives an event somewhere to travel
// on beyond whoever it arrived from.
func (m *Membership) handleEnvelope(fields []string, arrivalConn net.Conn) {
	if len(fields) < 4 {
		return
	}
	kind, originIP, originPortStr, seqStr := fields[0], fields[1], fields[2], fields[3]
	originPort, err := strconv.Atoi(originPortStr)
	if err != nil {
		return
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return
	}
	payload := fields[4:]

	if originIP == m.self.IP && originPort == m.self.Port {
		return // absorb: this is our own event, back around the ring
	}

	key := fmt.Sprintf("%s:%d", originIP, originPort)
	m.mu.Lock()
	if seq <= m.lastSeq[key] {
		m.mu.Unlock()
		return // already processed (duplicate / out-of-order retransmit)
	}
	m.lastSeq[key] = seq
	m.mu.Unlock()

	// Forward BEFORE applying: applyEnvelope reconciles on ADD/REMOVE,
	// which can tear down exactly the connection this relay still needs —
	// the same broadcast-before-reconcile rule as handleJoin/dialRight
	// (#759 problem 2). The forwarded line is the untouched original, so
	// nothing about the local apply feeds into it; dedup was already
	// recorded above, so a concurrent redelivery can't double-forward.
	m.broadcastRing(arrivalConn, strings.Join(fields, "\t"))
	m.applyEnvelope(kind, payload)
}

func (m *Membership) applyEnvelope(kind string, payload []string) {
	switch kind {
	case "DIRECTOR-ADD":
		if len(payload) < 2 {
			return
		}
		port, err := strconv.Atoi(payload[1])
		if err != nil {
			return
		}
		m.addMember(Member{IP: payload[0], Port: port})
		m.reconcile()
	case "DIRECTOR-REMOVE":
		if len(payload) < 2 {
			return
		}
		port, err := strconv.Atoi(payload[1])
		if err != nil {
			return
		}
		m.removeMember(Member{IP: payload[0], Port: port})
		m.reconcile()
	case "RING-CHANGE":
		applyRingChangeFields(m.srv, payload)
	case "USER-MOVED":
		applyUserMovedFields(m.srv, payload)
	case "USER-ASSIGN":
		// payload: <hash> <backend> <assign_seq> <assign_by> (#772 PR-2)
		if m.srv == nil || len(payload) < 4 {
			return
		}
		hash, err := strconv.ParseUint(payload[0], 10, 32)
		if err != nil {
			return
		}
		seq, err := strconv.ParseUint(payload[2], 10, 64)
		if err != nil {
			return
		}
		if old := m.srv.userDir.MergeByHash(uint32(hash), payload[1], false, seq, payload[3]); old != "" {
			m.srv.kickStaleSessions(uint32(hash), old)
		}
	case "USER-KICKED":
		if len(payload) < 1 {
			return
		}
		user := payload[0]
		// Drop the sticky pin on every replica (#706). A move-kick carries the
		// old backend IP (#708): the pin is dropped only if it STILL points
		// there (compare-and-delete), so the move's fresh pin — set by the
		// USER-MOVED that flew with this kick — survives. A plain admin kick
		// (no old IP) clears unconditionally.
		if len(payload) >= 2 && payload[1] != "" {
			m.srv.userDir.DeleteIfBackend(user, payload[1])
		} else {
			m.srv.userDir.Delete(user)
		}
		m.srv.broadcastToLogins(fmt.Sprintf("USER-KICKED\t%s", user))
	case "SESSION-OPEN":
		// payload: <id> <user> <backend> <proto> (#804) — a remote replica of a
		// session another director owns; feeds the least_sessions load view.
		if m.srv != nil {
			m.srv.applyRemoteSessionOpen(payload)
		}
	case "SESSION-CLOSE":
		if m.srv != nil && len(payload) >= 1 {
			m.srv.applyRemoteSessionClose(payload[0])
		}
	case "BACKEND-UNREACHABLE":
		// payload: <backendIP> <reporterID> (#782). A proxy's unreachable
		// report gossiped by the director it reached; recorded under the
		// original reporter so corroboration aggregates ring-wide.
		if m.srv != nil {
			m.srv.applyRemoteUnreachable(payload)
		}
	case "USER-KILLING":
		// payload: <hash> <ttlMillis> (#847). Replicated as a DURATION; each
		// director computes its own local deadline so pod-clock skew cannot make
		// the hold unstable (the #772 wall-clock lesson).
		if m.srv != nil && len(payload) >= 2 {
			if hash, err := strconv.ParseUint(payload[0], 10, 32); err == nil {
				if ms, err := strconv.ParseInt(payload[1], 10, 64); err == nil {
					m.srv.applyKilling(uint32(hash), time.Duration(ms)*time.Millisecond)
				}
			}
		}
	case "USER-KILL-DONE":
		// payload: <hash> (#847) — kill confirmed or timed out; release the hold.
		if m.srv != nil && len(payload) >= 1 {
			if hash, err := strconv.ParseUint(payload[0], 10, 32); err == nil {
				m.srv.applyKillDone(uint32(hash))
			}
		}
	}
}

// broadcastRing writes line to every currently live ring connection except
// arrivalConn (nil for a locally-originated event, meaning skip nothing).
// This is the entire forward path — there is no per-role "the" connection
// to pick, so a connection's dial/accept origin and any topology change
// during its lifetime are irrelevant to whether it gets this event; see the
// ringConns field comment for why that replaced the old dialConn/passiveConn
// split. Best-effort: at N=1, or if ringConns is momentarily empty between a
// neighbor's death and a new connection completing, there's nowhere to send
// it — the gap is closed by the DIRECTOR-LIST snapshot exchanged on every
// (re)connect, not by queuing here. Snapshots the connection set under
// rightMu and writes outside the lock, with a bounded per-write deadline, so
// one slow/stuck peer can't stall delivery to the others.
func (m *Membership) broadcastRing(arrivalConn net.Conn, line string) {
	m.rightMu.Lock()
	conns := make([]net.Conn, 0, len(m.ringConns))
	for c := range m.ringConns {
		if c != arrivalConn {
			conns = append(conns, c)
		}
	}
	m.rightMu.Unlock()

	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprintf(c, "%s\n", line)
		_ = c.SetWriteDeadline(time.Time{})
	}
}

// ---- ring keepalive: PING both neighbors, reference-style (#768) ----------

// ringPingInterval / ringPingTimeout pace the per-connection ring
// keepalive, reusing the same knobs as the client plane (the reference
// similarly derives its ring PING from director_ping_idle_timeout).
func (m *Membership) ringPingInterval() time.Duration { return m.srv.opts.pingInterval() }
func (m *Membership) ringPingTimeout() time.Duration  { return m.srv.opts.pingTimeout() }

// ringPinger writes a PING line on conn every ring ping interval until the
// connection dies or ctx ends — the reference PINGs BOTH its neighbor
// connections (director.c: director_connection_ping(dir->left) +
// (dir->right)), and both our read loops enforce a read deadline of
// interval+timeout, so a silently-hung peer (no RST, no FIN) is detected
// within one interval+timeout on whichever side notices first, instead of
// waiting for the OS to eventually surface the dead TCP session.
func (m *Membership) ringPinger(ctx context.Context, conn net.Conn) {
	interval := m.ringPingInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(conn, "PING\n"); err != nil {
			return // connection gone — its read loop handles the fallout
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}
}

// onLeftConnLost handles the loss of the accepted connection from this
// node's LEFT neighbor (#768) — the reference monitors both dir->left and
// dir->right, so a death is always detected by the dead node's immediate
// neighbors (O(1) probes per node, regardless of ring size) rather than by
// an all-probes-all sweep. dialRight already owns right-side deaths; this
// is the symmetric left side, with the same verify-then-declare shape:
// losing an inbound connection is often benign (the left neighbor
// re-targeted its dial after a membership change and closed the old one),
// so the dialer identity is checked against the CURRENT computed left
// neighbor, and death is declared only after a few failed verification
// probes. In the reference a lost connection alone never removes a host —
// its host list is static config, so it just reconnects around the hole —
// but our membership is dynamic k8s pods whose IPs never come back, so the
// correct adaptation is eviction (with the reference's own delayed-purge
// "removed" trick, our tombstones, preventing gossip re-adds); a live
// member wrongly suspected simply passes a probe (or rejoins on its own
// next poll, clearing the tombstone).
func (m *Membership) onLeftConnLost(ctx context.Context, dialer Member) {
	if dialer.isZero() || ctx == nil || ctx.Err() != nil {
		return
	}
	left, ok := m.leftNeighbor()
	if !ok || !left.equal(dialer) {
		return // not our left (anymore) — a benign re-target or an old conn
	}

	const maxAttempts = 3
	timeout := m.probeTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		err := m.joinVia(pctx, dialer.String())
		cancel()
		if err == nil {
			return // alive — it will re-dial us on its own
		}
		// Someone else may have already declared (and propagated) this
		// death while we were probing.
		still := false
		for _, mem := range m.Members() {
			if mem.equal(dialer) {
				still = true
				break
			}
		}
		if !still {
			return
		}
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
	slog.Warn("director: left ring neighbor unreachable, declaring dead", "self", m.self, "target", dialer)
	m.removeMember(dialer)
	m.originate("DIRECTOR-REMOVE", fmt.Sprintf("%s\t%d", dialer.IP, dialer.Port))
	m.reconcile()
}

// ---- accept side: serving an already-accepted ring/PEER connection --------

// serveRingConn processes an accepted ring connection until it closes. rd
// is the SAME reader handleConn used for the handshake — reusing it (rather
// than wrapping conn in a fresh bufio.Reader) is required to not drop any
// bytes the handshake read already buffered ahead. The connection is
// registered in ringConns for its whole lifetime, regardless of dial/accept
// role or N=2/N=3+ topology — see the field comment.
//
// Loss of this connection is a death SIGNAL when its dialer is this node's
// current left neighbor (#768) — see onLeftConnLost; verification probes
// keep a benign re-target or reconnect from being mistaken for a death.
func (m *Membership) serveRingConn(conn net.Conn, rd *bufio.Reader, dialer Member) {
	m.rightMu.Lock()
	m.ringConns[conn] = ringConnMeta{peer: dialer, since: time.Now(), role: "accept"}
	m.rightMu.Unlock()
	defer func() {
		m.rightMu.Lock()
		delete(m.ringConns, conn)
		delete(m.backendHashMiss, conn)
		delete(m.backendSyncAt, conn)
		m.rightMu.Unlock()
	}()

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go m.ringPinger(ctx, conn)

	// Mirror connectRight's snapshot exchange from this side too, so
	// convergence after a race doesn't depend on which end happened to
	// dial — see the comment there for why this resync exists.
	_, _ = fmt.Fprintf(conn, "DIRECTOR-LIST\t%s\t%s\n", formatMemberList(m.Members()), formatMemberList(m.removedList()))
	m.sendUserSnapshot(conn) // #772: userDir snapshot on connect

	readWindow := m.ringPingInterval() + m.ringPingTimeout()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readWindow))
		line, err := readBoundedLine(rd)
		if err != nil {
			m.onLeftConnLost(ctx, dialer)
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		switch {
		case fields[0] == "PING":
			_, _ = fmt.Fprintf(conn, "PONG\n")
		case fields[0] == "PONG":
			// keepalive traffic — its arrival already refreshed the
			// read deadline, nothing else to do
		case fields[0] == "QUIT":
			// Deliberate close announced by the peer (#768, reference
			// parity) — typically our left neighbor re-targeting its
			// dial after a membership change. Explicitly NOT a death
			// signal: return without onLeftConnLost, skipping the
			// verification probes an unannounced drop would trigger.
			reason := ""
			if len(fields) >= 2 {
				reason = fields[1]
			}
			slog.Debug("director: ring peer quit", "dialer", dialer, "reason", reason)
			return
		case fields[0] == "DIRECTOR-LIST" && len(fields) >= 2:
			removed := ""
			if len(fields) >= 3 {
				removed = fields[2]
			}
			m.mergeMembers(parseMemberList(fields[1]), parseMemberList(removed))
		case fields[0] == "USER":
			m.applyUserLine(fields)
		case strings.HasPrefix(fields[0], "BACKEND-") && m.handleBackendSyncLine(conn, rd, fields):
			// #846 backend-set resync sub-protocol (per-connection request/response)
		default:
			m.handleRingLine(fields, conn)
		}
	}
}

// ---- admin ring status (#833) ----------------------------------------------

// RingStatus is one replica's view of the ring, built from its own membership
// state — the per-replica nature is deliberate: comparing several replicas'
// RingStatus is how divergence (the #750-era failure mode) is spotted.
type RingStatus struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Self          string              `json:"self"`
	Size          int                 `json:"size"`
	Members       []RingMemberStatus  `json:"members"`
	Tombstones    []RingTombstoneInfo `json:"tombstones"`
	// BackendSetHash is a stable hash of this replica's ROUTING backend set
	// (#846). Two replicas that agree on routing produce the same hash;
	// divergence between replicas is a dropped RING-CHANGE that ring status
	// --all now flags (and PR-2 will auto-heal). Empty only when there is no
	// ring to hash (Status built without a Server, in unit tests).
	BackendSetHash string `json:"backendSetHash"`
}

// RingMemberStatus describes one member as this replica sees it. Left/Right are
// the member's computed neighbors in (ip,port) order (nil at N=1). Link is set
// only when the member is a direct ring-neighbor of THIS replica — it carries
// the live edge's state and uptime.
type RingMemberStatus struct {
	Addr  string    `json:"addr"`
	Index int       `json:"index"`
	Self  bool      `json:"self"`
	Left  *string   `json:"left"`
	Right *string   `json:"right"`
	Seq   *uint64   `json:"seq"` // dedup watermark: highest seq processed from this origin; nil = none heard
	Link  *RingLink `json:"link"`
}

// RingLink is this replica's live ring edge to a neighbor.
type RingLink struct {
	Role  string  `json:"role"`  // "left" | "right" | "both" (N=2, one conn serves both directions)
	State string  `json:"state"` // "connected" | "reconnecting"
	Since *string `json:"since"` // RFC3339 connection-established time; nil when reconnecting
}

// RingTombstoneInfo is a member known-dead on this replica and the age of that
// tombstone (how long ago this node learned of the death).
type RingTombstoneInfo struct {
	Addr string `json:"addr"`
	Age  string `json:"age"`
}

// Status builds this replica's RingStatus snapshot. Read-only.
func (m *Membership) Status() RingStatus {
	m.mu.RLock()
	members := make([]Member, len(m.members))
	copy(members, m.members)
	lastSeq := make(map[string]uint64, len(m.lastSeq))
	for k, v := range m.lastSeq {
		lastSeq[k] = v
	}
	now := time.Now()
	tombs := make([]RingTombstoneInfo, 0, len(m.removed))
	for mem, t := range m.removed {
		tombs = append(tombs, RingTombstoneInfo{Addr: mem.String(), Age: now.Sub(t).Round(time.Second).String()})
	}
	m.mu.RUnlock()

	sort.Slice(members, func(i, j int) bool { return members[i].less(members[j]) })
	sort.Slice(tombs, func(i, j int) bool { return tombs[i].Addr < tombs[j].Addr })

	selfLeft, hasLeft := m.leftNeighbor()
	selfRight, hasRight := m.rightNeighbor()

	// peer -> live edge metadata, so a member row can report its link state.
	m.rightMu.Lock()
	connByPeer := make(map[Member]ringConnMeta, len(m.ringConns))
	for _, meta := range m.ringConns {
		connByPeer[meta.peer] = meta
	}
	m.rightMu.Unlock()

	n := len(members)
	rows := make([]RingMemberStatus, 0, n)
	for i, mem := range members {
		row := RingMemberStatus{Addr: mem.String(), Index: i, Self: mem.equal(m.self)}
		if n >= 2 {
			l := members[(i-1+n)%n].String()
			r := members[(i+1)%n].String()
			row.Left, row.Right = &l, &r
		}
		if s, ok := lastSeq[mem.String()]; ok {
			v := s
			row.Seq = &v
		}

		if !mem.equal(m.self) {
			role := ""
			switch {
			case n == 2:
				// One connection serves both directions between the pair.
				role = "both"
			case hasLeft && selfLeft.equal(mem) && hasRight && selfRight.equal(mem):
				role = "both"
			case hasLeft && selfLeft.equal(mem):
				role = "left"
			case hasRight && selfRight.equal(mem):
				role = "right"
			}
			if role != "" {
				link := &RingLink{Role: role, State: "reconnecting"}
				if meta, ok := connByPeer[mem]; ok {
					link.State = "connected"
					since := meta.since.UTC().Format(time.RFC3339)
					link.Since = &since
				}
				row.Link = link
			}
		}
		rows = append(rows, row)
	}

	hash := ""
	if m.srv != nil {
		hash = backendSetHash(m.srv.ring.Backends())
	}

	return RingStatus{
		SchemaVersion:  1,
		Self:           m.self.String(),
		Size:           n,
		Members:        rows,
		Tombstones:     tombs,
		BackendSetHash: hash,
	}
}
