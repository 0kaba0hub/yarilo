package director

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Confirmed ring-wide kick (#847). When a user is kicked or moved, the director
// marks the user "killing" and holds LOOKUP for it, so a concurrent login cannot
// be assigned a fresh backend while the old sessions are still being torn down
// elsewhere — closing the split-writer window (#788). The kill is confirmed
// complete when the user's RING-WIDE session count (the #804 replicated registry)
// has stayed at zero for the confirm grace, or the hard timeout elapses
// (fallthrough, so a stuck holder never locks a user out).
//
// killReason=killing is returned to the login proxy as a RETRYABLE lookup
// failure — the proxy re-LOOKUPs (bounded) rather than erroring the client.

// killState is one user's kill-in-progress record. deadline is computed LOCALLY
// from a replicated DURATION, never a wall-clock deadline on the wire (pod-clock
// skew, the #772 lesson). zeroSince is when the ring-wide session count was first
// observed at zero (zero value = not yet); the confirm requires it to hold for
// userKillConfirmGrace so a momentary SESSION-OPEN dip cannot clear prematurely.
type killState struct {
	deadline  time.Time
	zeroSince time.Time
	// flush, when non-nil, is the per-user flush-hook context (#848) set ONLY by
	// the director that originates a deliberate relocation (moveUser or a graceful
	// evacuation). Peers receiving the replicated kill never set it, so only the
	// originator runs the hook, fired from the confirmed-kill sweep after the old
	// sessions are gone.
	flush *flushCtx
}

// flushCtx carries what the flush hook is passed for one relocated user (#848).
type flushCtx struct {
	user       string
	oldBackend string
	newBackend string
}

// killReason is the retryable LOOKUP failure reason a held user gets.
const killReason = "killing"

// startKilling marks user (by hash) killing locally and replicates it ring-wide
// as a DURATION, so every director gates LOOKUP for the same user. Called before
// the sessions are torn down.
func (s *Server) startKilling(hash uint32) {
	ttl := s.opts.userKillTimeout()
	// #870: arm the confirm immediately when the user is ALREADY quiesced (no
	// sessions ring-wide at kill-start). Otherwise zeroSince — only set on a
	// transition to zero via SESSION-CLOSE — would never arm for a user with
	// nothing to close, so the kill could exit only via the hard timeout (skipping
	// the #848 flush and lingering the LOOKUP hold the full user_kill_timeout). A
	// session opening before the grace disarms it again via noteSessionOpened.
	// Computed before killMu to avoid nesting (userSessionCount takes sessRecMu).
	quiesced := s.userSessionCount(hash) == 0
	s.killMu.Lock()
	st := killState{deadline: time.Now().Add(ttl)}
	if quiesced {
		st.zeroSince = time.Now()
	}
	s.killing[hash] = st
	s.killMu.Unlock()
	// Replicate the TTL in milliseconds; receivers compute their own deadline.
	s.membership.originate("USER-KILLING", fmt.Sprintf("%d\t%d", hash, ttl.Milliseconds()))
}

// attachFlush records the flush-hook context on an in-progress kill (#848) so
// the sweep can run the per-user hook once the old sessions are gone. No-op when
// no flush_program is configured or the kill record is already gone.
func (s *Server) attachFlush(hash uint32, ctx flushCtx) {
	if s.opts.FlushProgram == "" {
		return
	}
	s.killMu.Lock()
	if st, ok := s.killing[hash]; ok {
		st.flush = &ctx
		s.killing[hash] = st
	}
	s.killMu.Unlock()
}

// applyKilling records a replicated kill for hash with the given TTL, computing
// the deadline against THIS director's clock.
func (s *Server) applyKilling(hash uint32, ttl time.Duration) {
	s.killMu.Lock()
	// Keep an existing (possibly already-armed) record's zeroSince rather than
	// resetting it, but always refresh the deadline to the newer TTL.
	st := s.killing[hash]
	st.deadline = time.Now().Add(ttl)
	s.killing[hash] = st
	s.killMu.Unlock()
}

// applyKillDone clears a replicated kill-complete for hash.
func (s *Server) applyKillDone(hash uint32) {
	s.killMu.Lock()
	delete(s.killing, hash)
	s.killMu.Unlock()
}

// isKilling reports whether LOOKUP for hash must be held. A record past its
// deadline is treated as cleared (lazy fallthrough — the sweep also clears it).
func (s *Server) isKilling(hash uint32) bool {
	s.killMu.Lock()
	defer s.killMu.Unlock()
	st, ok := s.killing[hash]
	if !ok {
		return false
	}
	if time.Now().After(st.deadline) {
		return false
	}
	return true
}

// noteSessionOpened re-arms the confirm for a killing user: a session opening
// means the count is not stably zero, so any pending zero observation is void.
func (s *Server) noteSessionOpened(user string) {
	hash := HashUsername(user, s.hf)
	s.killMu.Lock()
	if st, ok := s.killing[hash]; ok {
		st.zeroSince = time.Time{}
		s.killing[hash] = st
	}
	s.killMu.Unlock()
}

// noteSessionClosed arms the confirm for a killing user once its ring-wide
// session count reaches zero.
func (s *Server) noteSessionClosed(user string) {
	hash := HashUsername(user, s.hf)
	s.killMu.Lock()
	st, ok := s.killing[hash]
	s.killMu.Unlock()
	if !ok {
		return
	}
	if s.userSessionCount(hash) != 0 {
		return
	}
	s.killMu.Lock()
	// Only arm on the transition to zero, don't push the timestamp forward.
	if st, ok = s.killing[hash]; ok && st.zeroSince.IsZero() {
		st.zeroSince = time.Now()
		s.killing[hash] = st
	}
	s.killMu.Unlock()
}

// userSessionCount counts this director's ring-wide view of active sessions for
// the user hash (local + #804 remote replicas).
func (s *Server) userSessionCount(hash uint32) int {
	s.sessRecMu.RLock()
	defer s.sessRecMu.RUnlock()
	n := 0
	for _, rec := range s.sessById {
		if HashUsername(rec.user, s.hf) == hash {
			n++
		}
	}
	return n
}

// StartKillSweep runs the confirm/timeout sweep: it clears a kill when the
// ring-wide session count has stayed at zero for the confirm grace, or the hard
// timeout has elapsed (fallthrough, logged), gossiping USER-KILL-DONE so every
// director resumes LOOKUP together.
func (s *Server) StartKillSweep(ctx context.Context) {
	grace := s.opts.userKillConfirmGrace()
	tick := grace / 2
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepKills(grace)
			}
		}
	}()
}

type confirmedKill struct {
	hash  uint32
	flush *flushCtx // #848: per-user hook context, non-nil only on originator moves
}

func (s *Server) sweepKills(grace time.Duration) {
	now := time.Now()
	var timedOut []uint32
	var confirmed []confirmedKill

	s.killMu.Lock()
	for hash, st := range s.killing {
		switch {
		case now.After(st.deadline):
			timedOut = append(timedOut, hash)
			delete(s.killing, hash)
		case !st.zeroSince.IsZero() && now.Sub(st.zeroSince) >= grace:
			confirmed = append(confirmed, confirmedKill{hash: hash, flush: st.flush})
			delete(s.killing, hash)
		}
	}
	s.killMu.Unlock()

	for _, hash := range timedOut {
		slog.Warn("director: kill not confirmed within timeout, releasing LOOKUP hold (fallthrough)", "hash", hash)
		s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", hash))
		s.evacKillDone(hash) // a timed-out move still frees its evacuation slot (#849)
	}
	for _, ck := range confirmed {
		slog.Info("director: kill confirmed ring-wide, releasing LOOKUP hold", "hash", ck.hash)
		s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", ck.hash))
		s.evacKillDone(ck.hash) // #849: confirmed move frees its slot; pull the next user
		if ck.flush != nil {
			// #848: old sessions are now gone — run the operator cleanup hook.
			// Async + best-effort; never gates the ring.
			s.runFlushHook(ck.hash, *ck.flush)
		}
	}
}
