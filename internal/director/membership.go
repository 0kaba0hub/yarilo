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
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
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

// less orders members deterministically: IP first (string compare), then
// port. Every node computes the exact same ordering from the exact same
// member set, with zero coordination — that determinism is what makes the
// ring computable locally instead of needing a vote.
func (m Member) less(o Member) bool {
	if m.IP != o.IP {
		return m.IP < o.IP
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

	mu      sync.RWMutex
	members []Member          // sorted, includes self once Start has run
	lastSeq map[string]uint64 // "ip:port" -> highest seq processed (dedup)
	seq     atomic.Uint64     // this node's own outgoing seq counter

	rightMu     sync.Mutex
	rightTarget Member // zero Member{} = no active dial target
	// dialConn is the connection WE dial out (set by connectRight, managed
	// by reconcile's dial lifecycle). passiveConn is set only for the N=2
	// tie-break's passive (higher-sorted) member: it never dials, so the
	// connection accepted FROM its one neighbor is its only send/receive
	// path (serveRingConn registers/clears it; reconcile never touches it).
	// forwardRight prefers dialConn, falling back to passiveConn — this
	// split exists because a 2→3 membership transition can leave a former
	// N=2-passive member suddenly needing its OWN dial-out (to its new
	// right neighbor) while the old accepted connection (still its correct
	// LEFT neighbor at N=3 too) must keep working right up until then.
	dialConn    net.Conn
	passiveConn net.Conn
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
	if len(seeds) == 0 {
		slog.Info("director: ring membership starting as singleton (no seeds configured)")
		return
	}
	go m.joinLoop(ctx, seeds)
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

// joinLoop tries each seed in turn, with a short backoff, until one join
// succeeds. It then returns — membership from that point on is maintained
// by DIRECTOR-ADD/REMOVE propagation, not further seed polling.
//
// Known phase-1 gap: two pods starting simultaneously before either can
// reach a seed (e.g. the first two pods behind a fresh ClusterIP) may each
// self-elect as a singleton ring and never merge, since neither retries
// after its own "join" trivially never happens. Anti-entropy (#750 phase 4)
// is the intended fix; not resolved here.
func (m *Membership) joinLoop(ctx context.Context, seeds []string) {
	backoff := 2 * time.Second
	for {
		for _, addr := range seeds {
			if ctx.Err() != nil {
				return
			}
			if err := m.joinVia(ctx, addr); err != nil {
				slog.Debug("director: ring join attempt failed", "seed", addr, "err", err)
				continue
			}
			slog.Info("director: joined ring", "seed", addr, "members", m.Members())
			m.reconcile()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
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
		return fmt.Errorf("director/join: rejected: %s", strings.Join(fields[1:], " "))
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
		return fmt.Errorf("director/join: rejected: %s", strings.Join(fields[1:], " "))
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

// setMembers replaces the member set with a deduplicated, sorted copy and
// triggers a reconcile so the right-neighbor dial matches the new topology.
func (m *Membership) setMembers(members []Member) {
	seen := make(map[Member]bool, len(members))
	uniq := make([]Member, 0, len(members))
	for _, mem := range members {
		if seen[mem] {
			continue
		}
		seen[mem] = true
		uniq = append(uniq, mem)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].less(uniq[j]) })

	m.mu.Lock()
	m.members = uniq
	m.mu.Unlock()
}

// mergeMembers unions incoming with the current member list (self always
// included) and, if that changes anything, applies it and reconciles. Used
// for the DIRECTOR-LIST resync exchanged on every ring connection — a
// plain union rather than a replace, so a snapshot that's simply stale
// (missing a member we already know about from elsewhere) can't regress us.
func (m *Membership) mergeMembers(incoming []Member) {
	current := m.Members()
	all := append(append([]Member{}, current...), incoming...)
	all = append(all, m.self)

	m.mu.Lock()
	before := len(m.members)
	m.mu.Unlock()

	m.setMembers(all)

	m.mu.RLock()
	after := len(m.members)
	m.mu.RUnlock()
	if after != before {
		m.reconcile()
	}
}

func (m *Membership) addMember(mem Member) {
	m.mu.Lock()
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

func (m *Membership) removeMember(mem Member) {
	m.mu.Lock()
	for i, existing := range m.members {
		if existing.equal(mem) {
			m.members = append(m.members[:i], m.members[i+1:]...)
			break
		}
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
	if err := writeLine(conn, "DIRECTOR-LIST\t"+formatMemberList(existing)); err != nil {
		return
	}
	_ = writeLine(conn, "DONE")

	slog.Info("director: ring join accepted", "joiner", joiner, "members", len(existing)+1)
	joinAccepted.Inc()

	// Tell the rest of the ring about the new member, then adopt whatever
	// right-neighbor change that implies.
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
// case. Deliberately never touches passiveConn — an N=2 passive member's
// inbound connection stays valid (and in use) independent of whether this
// node currently has a dial target of its own; see the field comment.
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
	addr := target.String()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		redirect, err := m.connectRight(ctx, addr)
		if redirect != "" {
			addr = redirect
			attempt = 0 // a redirect is not a failed attempt
			continue
		}
		if err == nil {
			// connectRight only returns nil after ctx was cancelled
			// (clean shutdown, e.g. reconcile picked a new target).
			return
		}
		slog.Debug("director: ring dial attempt failed", "target", addr, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}
	slog.Warn("director: ring neighbor unreachable, declaring dead", "target", target)
	m.removeMember(target)
	m.originate("DIRECTOR-REMOVE", fmt.Sprintf("%s\t%d", target.IP, target.Port))
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
		"PEER\t1",
		"DONE",
	} {
		if _, wErr := fmt.Fprintf(conn, "%s\n", s); wErr != nil {
			return "", fmt.Errorf("handshake send: %w", wErr)
		}
	}

	m.rightMu.Lock()
	m.dialConn = conn
	m.rightMu.Unlock()
	defer func() {
		m.rightMu.Lock()
		if m.dialConn == conn {
			m.dialConn = nil
		}
		m.rightMu.Unlock()
	}()

	slog.Info("director: ring right-neighbor connected", "target", addr)

	// Exchange a full membership snapshot right away (#750 phase 1 fix): a
	// DIRECTOR-ADD fired between a join being accepted and that acceptor's
	// own dial to its (possibly just-changed) right neighbor completing
	// would otherwise race forwardRight's best-effort delivery and vanish
	// silently — this connection is brand new either way, so unconditional
	// resync on connect closes that window regardless of timing, without
	// needing the full user/backend state snapshot (#750 phase 3).
	if _, wErr := fmt.Fprintf(conn, "DIRECTOR-LIST\t%s\n", formatMemberList(m.Members())); wErr != nil {
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
			m.mergeMembers(parseMemberList(fields[1]))
		default:
			m.handleRingLine(fields)
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
	m.forwardRight(fmt.Sprintf("%s\t%s\t%d\t%d\t%s", kind, m.self.IP, m.self.Port, seq, payload))
}

// handleRingLine dispatches one line read from a ring connection: every
// envelope command (DIRECTOR-ADD/REMOVE, RING-CHANGE, USER-MOVED,
// USER-KICKED) goes through the shared (origin, seq) dedup + apply +
// forward path. There is no active ring PING loop yet in phase 1 — death
// detection is read-error-based (see dialRight) — so PING/PONG lines never
// occur on a ring connection today; #750 phase 4 adds anti-entropy over
// PING (needs the originating connection threaded through to reply on,
// unlike the other envelope kinds which always forward via dialConn).
func (m *Membership) handleRingLine(fields []string) {
	switch fields[0] {
	case "DIRECTOR-ADD", "DIRECTOR-REMOVE", "RING-CHANGE", "USER-MOVED", "USER-KICKED":
		m.handleEnvelope(fields)
	}
}

// handleEnvelope implements the propagation rule that makes the ring
// self-limiting with zero coordination: if the event's origin is this node
// itself, the event has travelled all the way around and is absorbed
// (applied once already, at origination — never re-applied, never
// forwarded again). Otherwise, apply locally (unless already seen — a
// safety net against out-of-order duplicates) and unconditionally forward
// to the right neighbor. "Unconditionally" matters at N=2: the right
// connection IS the same connection the event was just received on, so
// forwarding there is what completes the single round trip back to the
// origin for it to absorb — there is no separate "don't echo to sender"
// rule, and adding one would break N=2 entirely.
func (m *Membership) handleEnvelope(fields []string) {
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

	m.applyEnvelope(kind, payload)
	m.forwardRight(strings.Join(fields, "\t"))
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

// forwardRight writes line to this node's outgoing send path: its own
// dial-out connection when it has one, otherwise (the N=2 passive member
// only) the connection accepted from its one neighbor. Best-effort: at
// N=1, or between a neighbor's death and reconcile picking the next one,
// there's nowhere to send it — the gap is closed by state snapshot on the
// next (re)connect (#750 phase 3), not by queuing here.
func (m *Membership) forwardRight(line string) {
	m.rightMu.Lock()
	conn := m.dialConn
	if conn == nil {
		conn = m.passiveConn
	}
	m.rightMu.Unlock()
	if conn == nil {
		return
	}
	_, _ = fmt.Fprintf(conn, "%s\n", line)
}

// ---- accept side: serving an already-accepted ring/PEER connection --------

// serveRingConn processes an accepted ring connection until it closes. rd
// is the SAME reader handleConn used for the handshake — reusing it (rather
// than wrapping conn in a fresh bufio.Reader) is required to not drop any
// bytes the handshake read already buffered ahead.
//
// At N>=3 this connection is purely inbound (our left neighbor dialed us);
// forwarding an applied event goes out on our OWN separate dial-out
// connection (m.dialConn, set by connectRight) — never back down this one.
//
// At N=2, the lexicographically lower member is the only one who dials
// (rightNeighbor's tie-break); the higher member never has a dial-out
// connection of its own, so THIS accepted connection is registered as
// m.passiveConn for the duration — it is that member's only path in both
// directions, which is exactly what makes the N=2 single-round-trip
// origin-absorb behaviour work (see handleEnvelope). Registering it
// separately from dialConn (rather than reusing the same field) matters
// across a 2→3 transition: reconcile() only ever manages dialConn, so a
// former N=2-passive member picking up its own new right-neighbor dial (at
// N=3) can never accidentally tear down this still-valid inbound
// connection — see the struct field comment for the full reasoning.
//
// An accept-side disconnect never declares the peer dead — that's the
// dialing side's job (dialRight); losing an inbound connection here just
// means our neighbor will reconnect (or notice on its own dial that we're
// gone).
func (m *Membership) serveRingConn(conn net.Conn, rd *bufio.Reader, dialer Member) {
	isN2Passive := false
	if !dialer.isZero() {
		members := m.Members()
		if len(members) == 2 && dialer.equal(members[0]) && m.self.equal(members[1]) {
			isN2Passive = true
		}
	}
	if isN2Passive {
		m.rightMu.Lock()
		m.passiveConn = conn
		m.rightMu.Unlock()
		defer func() {
			m.rightMu.Lock()
			if m.passiveConn == conn {
				m.passiveConn = nil
			}
			m.rightMu.Unlock()
		}()
	}

	// Mirror connectRight's snapshot exchange from this side too, so
	// convergence after a race doesn't depend on which end happened to
	// dial — see the comment there for why this resync exists.
	_, _ = fmt.Fprintf(conn, "DIRECTOR-LIST\t%s\n", formatMemberList(m.Members()))

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
			m.mergeMembers(parseMemberList(fields[1]))
			continue
		}
		m.handleRingLine(fields)
	}
}
