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
// See INTERNALS.md §1 for the wire format.
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
	// antiEntropyInterval paces antiEntropyLoop's periodic snapshot
	// re-broadcast; 0 = default (3s), negative = disabled. See the
	// Options field comment.
	antiEntropyInterval time.Duration

	mu      sync.RWMutex
	members []Member          // sorted, includes self once Start has run
	lastSeq map[string]uint64 // "ip:port" -> highest seq processed (dedup)
	seq     atomic.Uint64     // this node's own outgoing seq counter
	// removed is the set of members known to be dead (#754) — a permanent
	// tombstone, not just a transient absence from the current member
	// list. Required because DIRECTOR-LIST resync (mergeMembers) unions
	// snapshots from potentially-stale peers: without a tombstone, a peer
	// who hasn't yet learned of a death would silently resurrect the
	// removed member on every reconnect. addMember clears the tombstone
	// for that (ip,port) — a legitimate fresh authenticated JOIN (or a
	// relayed DIRECTOR-ADD vouching for one) is trusted to mean exactly
	// that: this address is alive again, whether it's a rejoin or the
	// address was reassigned to a genuinely new pod.
	removed map[Member]struct{}

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
	// one the event just arrived on, matching Dovecot's director_update_send
	// (skip only the arrival connection; (origin, seq) dedup in
	// handleEnvelope is what actually stops the flood once it loops back to
	// its author — see INTERNALS.md). Earlier this repo instead kept a
	// single "the" forward path (dialConn, falling back to a passiveConn set
	// only for the N=2 tie-break's passive member) — that role was decided
	// once, when a connection was accepted, and never revisited as topology
	// changed on that same still-open connection: a 3→2 shrink could leave a
	// connection accepted under N=3 rules silently unable to forward
	// anything at all once its node became the N=2 passive side (#754
	// follow-up). ringConns has no such role to get stale — connections are
	// registered/deregistered purely by their own lifetime.
	dialConn    net.Conn
	ringConns   map[net.Conn]struct{}
	rightCancel context.CancelFunc

	ctx context.Context //nolint:containedctx // reconcile is triggered from accept-time handlers with no request-scoped ctx of their own; stored once at Start like Server's other background loops
}

// NewMembership creates a Membership for self, not yet started. secret
// authenticates incoming JOINs (empty = ring auth disabled, every JOIN
// rejected — #750 phase 2 adds dial-back + CIDR filtering on top of this
// HMAC core). tlsCfg wraps outgoing right-neighbor dials when non-nil.
// minMembers is an install-time warning threshold only; it never refuses
// service.
func NewMembership(srv *Server, self Member, secret []byte, tlsCfg *tls.Config, minMembers int) *Membership {
	return &Membership{
		srv:        srv,
		self:       self,
		secret:     secret,
		tlsCfg:     tlsCfg,
		minMembers: minMembers,
		lastSeq:    make(map[string]uint64),
		removed:    make(map[Member]struct{}),
		ringConns:  make(map[net.Conn]struct{}),
	}
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

// joinLoop tries each seed in turn until one join succeeds. It then
// returns — membership from that point on is maintained by
// DIRECTOR-ADD/REMOVE propagation plus the periodic anti-entropy snapshot
// (antiEntropyLoop), not further seed polling. A self-dial rejection
// retries fast (see errJoinSelfDial); every other failure walks the
// exponential backoff.
//
// Known phase-1 gap, narrowed but still real: several pods starting
// simultaneously behind a fresh ClusterIP can form two (or more) fully
// disjoint subrings that each converge internally and then stop retrying
// the seed, with no remaining path for either subring to discover the
// other — every retry after that lands on an already-known peer and looks
// like success. antiEntropyLoop heals any split where at least one
// connection crosses the two views; a split with zero crossing
// connections still needs #750 phase 4's seed re-poll / members_hash
// anti-entropy — not resolved here.
func (m *Membership) joinLoop(ctx context.Context, seeds []string) {
	backoff := 2 * time.Second
	for {
		selfDial := false
		for _, addr := range seeds {
			if ctx.Err() != nil {
				return
			}
			err := m.joinVia(ctx, addr)
			if err == nil {
				slog.Info("director: joined ring", "seed", addr, "members", m.Members())
				m.reconcile()
				return
			}
			if errors.Is(err, errJoinSelfDial) {
				selfDial = true
			}
			slog.Debug("director: ring join attempt failed", "seed", addr, "err", err)
		}
		wait := backoff
		if selfDial {
			// The seed answered (with ourselves) — it is reachable and
			// serving; kube-proxy just needs another roll of the dice.
			wait = 500 * time.Millisecond
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
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
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, m.tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("director/join: dial: %w", err)
	}
	defer conn.Close()

	rd := bufio.NewReaderSize(conn, 4096)
	if err := consumeServerHandshake(rd); err != nil {
		return fmt.Errorf("director/join: handshake: %w", err)
	}

	if _, err := fmt.Fprintf(conn, "DIRECTOR-JOIN\t%s\t%d\n", m.self.IP, m.self.Port); err != nil {
		return fmt.Errorf("director/join: send JOIN: %w", err)
	}

	line, err := rd.ReadString('\n')
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

	line, err = rd.ReadString('\n')
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

	line, err = rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("director/join: read member list: %w", err)
	}
	fields = strings.Split(strings.TrimRight(line, "\n"), "\t")
	if fields[0] != "DIRECTOR-LIST" || len(fields) < 2 {
		return fmt.Errorf("director/join: expected DIRECTOR-LIST, got %q", line)
	}
	members := parseMemberList(fields[1])
	members = append(members, m.self)
	if len(fields) >= 3 {
		for _, mem := range parseMemberList(fields[2]) {
			m.mu.Lock()
			m.removed[mem] = struct{}{}
			m.mu.Unlock()
		}
	}

	if _, err := rd.ReadString('\n'); err != nil { // DONE
		return fmt.Errorf("director/join: read DONE: %w", err)
	}

	m.setMembers(members)
	return nil
}

// consumeServerHandshake reads and discards VERSION..HOST-HAND-*..DONE. The
// join connection doesn't need the backend list — the real ring/PEER
// connection (dialRight) already learns it the same way client connections
// always have.
func consumeServerHandshake(rd *bufio.Reader) error {
	for {
		line, err := rd.ReadString('\n')
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
		if _, dead := m.removed[mem]; dead && !mem.equal(m.self) {
			continue
		}
		seen[mem] = true
		uniq = append(uniq, mem)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].less(uniq[j]) })
	m.members = uniq
	m.mu.Unlock()
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
		if !mem.equal(m.self) {
			m.removed[mem] = struct{}{}
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

// removedList returns a snapshot of the tombstone set, for exchange in a
// DIRECTOR-LIST resync.
func (m *Membership) removedList() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Member, 0, len(m.removed))
	for mem := range m.removed {
		out = append(out, mem)
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
		m.removed[mem] = struct{}{}
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
	joiner := Member{IP: fields[1]}
	port, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	joiner.Port = port

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
	line, err := rd.ReadString('\n')
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

	// Snapshot the member list BEFORE adding the joiner — DIRECTOR-LIST
	// tells the joiner who else is here; it adds itself locally.
	existing := m.Members()
	m.addMember(joiner)

	if err := writeLine(conn, "JOIN-OK"); err != nil {
		return
	}
	if err := writeLine(conn, "DIRECTOR-LIST\t"+formatMemberList(existing)+"\t"+formatMemberList(m.removedList())); err != nil {
		return
	}
	_ = writeLine(conn, "DONE")

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
		line, rErr := rd.ReadString('\n')
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
	m.ringConns[conn] = struct{}{}
	m.rightMu.Unlock()
	defer func() {
		m.rightMu.Lock()
		if m.dialConn == conn {
			m.dialConn = nil
		}
		delete(m.ringConns, conn)
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

	for {
		line, rErr := rd.ReadString('\n')
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
		case fields[0] == "CONNECT" && len(fields) >= 3:
			return net.JoinHostPort(fields[1], fields[2]), nil
		case fields[0] == "DIRECTOR-LIST" && len(fields) >= 2:
			removed := ""
			if len(fields) >= 3 {
				removed = fields[2]
			}
			m.mergeMembers(parseMemberList(fields[1]), parseMemberList(removed))
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

// handleRingLine dispatches one line read from a ring connection: every
// envelope command (DIRECTOR-ADD/REMOVE, RING-CHANGE, USER-MOVED,
// USER-KICKED) goes through the shared (origin, seq) dedup + apply +
// forward path. arrivalConn is the connection this line was just read from
// — handleEnvelope needs it to skip re-sending the event back the way it
// came. There is no active ring PING loop yet in phase 1 — death detection
// is read-error-based (see dialRight) — so PING/PONG lines never occur on a
// ring connection today; #750 phase 4 adds anti-entropy over PING.
func (m *Membership) handleRingLine(fields []string, arrivalConn net.Conn) {
	switch fields[0] {
	case "DIRECTOR-ADD", "DIRECTOR-REMOVE", "RING-CHANGE", "USER-MOVED", "USER-KICKED":
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
// Dovecot's director_update_send skip-arrival rule rather than a fixed
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
	case "USER-KICKED":
		if len(payload) < 1 {
			return
		}
		m.srv.broadcastToLogins(fmt.Sprintf("USER-KICKED\t%s", payload[0]))
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

// ---- accept side: serving an already-accepted ring/PEER connection --------

// serveRingConn processes an accepted ring connection until it closes. rd
// is the SAME reader handleConn used for the handshake — reusing it (rather
// than wrapping conn in a fresh bufio.Reader) is required to not drop any
// bytes the handshake read already buffered ahead. The connection is
// registered in ringConns for its whole lifetime, regardless of dial/accept
// role or N=2/N=3+ topology — see the field comment.
//
// An accept-side disconnect never declares the peer dead — that's the
// dialing side's job (dialRight); losing an inbound connection here just
// means our neighbor will reconnect (or notice on its own dial that we're
// gone).
func (m *Membership) serveRingConn(conn net.Conn, rd *bufio.Reader, dialer Member) {
	_ = dialer // no longer role-gates registration; CONNECT-redirect (checkRedirect) already used it before this call
	m.rightMu.Lock()
	m.ringConns[conn] = struct{}{}
	m.rightMu.Unlock()
	defer func() {
		m.rightMu.Lock()
		delete(m.ringConns, conn)
		m.rightMu.Unlock()
	}()

	// Mirror connectRight's snapshot exchange from this side too, so
	// convergence after a race doesn't depend on which end happened to
	// dial — see the comment there for why this resync exists.
	_, _ = fmt.Fprintf(conn, "DIRECTOR-LIST\t%s\t%s\n", formatMemberList(m.Members()), formatMemberList(m.removedList()))

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		if fields[0] == "DIRECTOR-LIST" && len(fields) >= 2 {
			removed := ""
			if len(fields) >= 3 {
				removed = fields[2]
			}
			m.mergeMembers(parseMemberList(fields[1]), parseMemberList(removed))
			continue
		}
		m.handleRingLine(fields, conn)
	}
}
