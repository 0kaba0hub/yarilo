package director

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// Backend-set auto-resync (#846 PR-2): each anti-entropy tick a director sends
// its backend-set hash to its ring neighbors (BACKEND-HASH). A neighbor whose
// hash differs for backendSyncMinMismatchTicks CONSECUTIVE ticks (debouncing a
// normal propagation window where up flips during flush/drain) pulls a full
// snapshot (BACKEND-SYNC-REQ → BACKEND-HAND-START … BACKEND-HOST …
// BACKEND-HAND-END) and merges it under the lease rules.
//
// PAIRWISE, not full-mesh: the hash is exchanged only between directly-connected
// neighbors. Non-adjacent divergence heals TRANSITIVELY (A↔B, then B↔C),
// converging in ~N/2 ticks. Do NOT broadcast ring-wide: pairwise exchange keeps
// steady-state cost at one line per connection per tick.
//
// MERGE IS ADD-ONLY and lease-gated, never a resurrection channel (#776):
//   - a lease-managed record (seq>0) applies only if STRICTLY NEWER than our
//     lease (recordBackendSeen), exactly like a live heartbeat;
//   - a static record (seq==0) applies only if the backend is locally ABSENT;
//   - a record for a recently-removed backend is blocked unless its seq exceeds
//     the removal seq, so a peer lagging a removal cannot re-add the ghost.
// Absence from a snapshot is NEVER authoritative: a missed-DOWN self-heals via
// the lease TTL, so this resync targets only the missed-UP that otherwise never
// heals (a backend self-registers to just one director).

const (
	// backendSyncMinMismatchTicks debounces resync against normal propagation:
	// a mismatch must persist this many consecutive anti-entropy ticks before a
	// snapshot is pulled.
	backendSyncMinMismatchTicks = 2
	// backendSyncCooldown rate-limits snapshot pulls per connection so a
	// flapping backend cannot drive a resync storm.
	backendSyncCooldown = 10 * time.Second
	// backendTombTTL bounds how long a removed backend blocks resurrection by a
	// stale snapshot. Comfortably longer than propagation + a few ticks.
	backendTombTTL = 5 * time.Minute
)

// backendTombstone records a removed backend and the lease seq it had at
// removal, so a snapshot record must prove a strictly-newer registration to
// re-admit it.
type backendTombstone struct {
	seq uint64
	at  time.Time
}

// broadcastBackendHash sends this node's backend-set hash to every ring
// neighbor. Called from the anti-entropy tick.
func (m *Membership) broadcastBackendHash() {
	if m.srv == nil {
		return
	}
	m.broadcastRing(nil, "BACKEND-HASH\t"+backendSetHash(m.srv.ring.Backends()))
}

// handleBackendSyncLine handles the resync sub-protocol on a ring connection,
// returning true when it consumed the line. conn carries replies; rd reads a
// snapshot block. Kept out of handleRingLine: these are per-connection
// request/response lines, not (origin,seq) envelopes to forward.
func (m *Membership) handleBackendSyncLine(conn net.Conn, rd *bufio.Reader, fields []string) bool {
	switch fields[0] {
	case "BACKEND-HASH":
		if len(fields) >= 2 {
			m.onBackendHash(conn, fields[1])
		}
		return true
	case "BACKEND-SYNC-REQ":
		m.sendBackendSnapshot(conn)
		return true
	case "BACKEND-HAND-START":
		m.recvBackendSnapshot(rd)
		return true
	}
	return false
}

// onBackendHash compares a neighbor's backend-set hash to ours and, on a
// sustained mismatch past the debounce (and outside the cooldown), asks that
// neighbor for a full snapshot.
func (m *Membership) onBackendHash(conn net.Conn, peerHash string) {
	if m.srv == nil {
		return
	}
	own := backendSetHash(m.srv.ring.Backends())

	m.rightMu.Lock()
	if peerHash == own {
		delete(m.backendHashMiss, conn)
		m.rightMu.Unlock()
		return
	}
	m.backendHashMiss[conn]++
	miss := m.backendHashMiss[conn]
	cooled := time.Since(m.backendSyncAt[conn]) >= backendSyncCooldown
	request := miss >= backendSyncMinMismatchTicks && cooled
	if request {
		m.backendSyncAt[conn] = time.Now()
		m.backendHashMiss[conn] = 0
	}
	m.rightMu.Unlock()

	if request {
		writeRingLine(conn, "BACKEND-SYNC-REQ")
		slog.Debug("director: backend-set hash mismatch, pulling snapshot", "peer_hash", peerHash, "own_hash", own)
	}
}

// sendBackendSnapshot writes our full backend set as a BACKEND-HAND block, each
// record carrying its lease seq (0 = static / no heartbeat) so the receiver can
// merge under lease rules.
func (m *Membership) sendBackendSnapshot(conn net.Conn) {
	if m.srv == nil {
		return
	}
	backends := m.srv.ring.Backends()
	writeRingLine(conn, "BACKEND-HAND-START")
	for _, b := range backends {
		writeRingLine(conn, fmt.Sprintf("BACKEND-HOST\t%s\t%d\t%s\t%d\t%t\t%d",
			b.IP, b.Port, b.Tag, b.Vhosts, b.Up, m.srv.backendLeaseSeq(b.IP)))
	}
	writeRingLine(conn, "BACKEND-HAND-END")
}

// recvBackendSnapshot reads a BACKEND-HAND block (already past START) and merges
// it. Reads inline off the ring reader — the block is sent contiguously.
func (m *Membership) recvBackendSnapshot(rd *bufio.Reader) {
	var recs []backendRecord
	for {
		line, err := readBoundedLine(rd)
		if err != nil {
			return
		}
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if f[0] == "BACKEND-HAND-END" {
			break
		}
		if f[0] == "BACKEND-HOST" && len(f) >= 7 {
			port, e1 := strconv.Atoi(f[2])
			vhosts, e2 := strconv.Atoi(f[4])
			up := f[5] == "true"
			seq, e3 := strconv.ParseUint(f[6], 10, 64)
			if e1 != nil || e2 != nil || e3 != nil {
				continue
			}
			recs = append(recs, backendRecord{ip: f[1], port: port, tag: f[3], vhosts: vhosts, up: up, seq: seq})
		}
	}
	if m.srv != nil {
		m.srv.mergeBackendSnapshot(recs)
	}
}

// backendRecord is one snapshot entry.
type backendRecord struct {
	ip     string
	port   int
	tag    string
	vhosts int
	up     bool
	seq    uint64
}

// mergeBackendSnapshot applies a neighbor's snapshot, ADD-only and lease-gated.
func (s *Server) mergeBackendSnapshot(recs []backendRecord) {
	changed := false
	for _, r := range recs {
		if s.applyBackendRecord(r) {
			changed = true
		}
	}
	if changed {
		s.updateMetrics()
	}
}

// applyBackendRecord merges one snapshot record under the lease/tombstone rules
// (see the file header). Returns true if it changed the ring.
func (s *Server) applyBackendRecord(r backendRecord) bool {
	if s.backendTombActive(r.ip, r.seq) {
		return false // recently removed; a stale snapshot must not resurrect it
	}
	if r.seq > 0 {
		// Lease-managed: apply only if strictly newer than our lease.
		if !s.recordBackendSeen(r.ip, r.seq) {
			return false
		}
		s.ring.AddBackend(&ring.Backend{IP: r.ip, Port: r.port, Tag: r.tag, Up: r.up, Vhosts: r.vhosts, LastUp: time.Now().Unix()})
		s.clearBackendTomb(r.ip)
		slog.Info("director: backend healed via resync snapshot", "ip", r.ip, "tag", r.tag, "up", r.up, "seq", r.seq)
		return true
	}
	// Static record (no lease seq): apply only if the backend is locally absent.
	if s.ring.GetBackend(r.ip) == nil {
		s.ring.AddBackend(&ring.Backend{IP: r.ip, Port: r.port, Tag: r.tag, Up: r.up, Vhosts: r.vhosts})
		slog.Info("director: static backend healed via resync snapshot", "ip", r.ip, "tag", r.tag)
		return true
	}
	return false
}

// backendLeaseSeq returns the last heartbeat seq recorded for ip (0 if none —
// a static / admin-added backend that never heartbeats).
func (s *Server) backendLeaseSeq(ip string) uint64 {
	s.backendSeenMu.Lock()
	defer s.backendSeenMu.Unlock()
	return s.backendSeen[ip].seq
}

// recordBackendTomb marks ip as removed at its current lease seq, blocking
// resurrection by a stale snapshot until backendTombTTL elapses or a
// strictly-newer record proves a genuine re-registration.
func (s *Server) recordBackendTomb(ip string) {
	s.backendTombMu.Lock()
	s.backendTomb[ip] = backendTombstone{seq: s.backendLeaseSeq(ip), at: time.Now()}
	s.backendTombMu.Unlock()
}

func (s *Server) clearBackendTomb(ip string) {
	s.backendTombMu.Lock()
	delete(s.backendTomb, ip)
	s.backendTombMu.Unlock()
}

// backendTombActive reports whether ip is still tombstoned against a snapshot
// record carrying recSeq — true (blocked) when the tombstone is fresh and
// recSeq does not exceed the removal seq (a static seq==0 never exceeds it).
func (s *Server) backendTombActive(ip string, recSeq uint64) bool {
	s.backendTombMu.Lock()
	defer s.backendTombMu.Unlock()
	tomb, ok := s.backendTomb[ip]
	if !ok {
		return false
	}
	if time.Since(tomb.at) > backendTombTTL {
		delete(s.backendTomb, ip)
		return false
	}
	return recSeq <= tomb.seq
}

// writeRingLine writes one line to a ring connection under a bounded deadline,
// matching broadcastRing's best-effort write discipline.
func writeRingLine(conn net.Conn, line string) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "%s\n", line)
	_ = conn.SetWriteDeadline(time.Time{})
}
