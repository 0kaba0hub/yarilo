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
	// originated is true only on the director that STARTED this kill. It decides
	// who may announce the end of it.
	//
	// Every member keeps its own record and sweeps it independently, so before
	// this field whichever member's sweep finished first broadcast
	// USER-KILL-DONE, and applyKillDone deleted the record everywhere -- taking
	// the originator's flush context with it. The hook then ran only when the
	// originator happened to win a three-way race, roughly one move in three
	// (#1359).
	//
	// Announcing is now the originator's alone. A peer still clears its own
	// hold, by its own confirm or its own deadline; it simply does not decide
	// for anyone else. That keeps "confirmed" meaning what the hook needs it to
	// mean -- the director that started the move saw the sessions go, itself --
	// rather than trusting another member's view of a state each sees
	// separately.
	originated bool
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
	st := killState{deadline: time.Now().Add(ttl), originated: true}
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
	// originated is deliberately not set here: this record came from a peer.
	// A member that did not start the kill does not announce its end.
	s.killing[hash] = st
	s.killMu.Unlock()
}

// applyKillDone clears a replicated kill-complete for hash.
//
// The announcement releases this member's LOOKUP hold, which is its whole
// purpose. But if the record being cleared is OURS and carries a flush context,
// dropping it silently would take the hook with it -- which is the defect
// (#1359) arriving by another door: during a rolling upgrade the peers that
// have not been updated still announce, so the originator's record is still
// deleted by somebody else.
//
// So the hook runs here too, on one condition: THIS member must already have
// seen the sessions go.
//
// This path exists for the mixed ring and becomes unreachable once no member
// predating #1359 can be in it -- at which point it should be removed rather
// than left as a second place the hook can fire from. That keeps the contract the hook is written to -- it
// runs after the old sessions are gone, observed locally -- while an
// announcement arriving over a still-live session (a peer's fallthrough) drops
// the context without running anything, exactly as before.
func (s *Server) applyKillDone(hash uint32) {
	s.killMu.Lock()
	st, ok := s.killing[hash]
	delete(s.killing, hash)
	s.killMu.Unlock()
	if !ok || !st.originated || st.flush == nil || st.zeroSince.IsZero() {
		return
	}
	slog.Info("director: kill announced by a peer while ours was pending; running the flush hook",
		"hash", hash)
	s.runFlushHook(hash, *st.flush)
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

// killOutcome is one record the sweep finished with, either way it ended:
// confirmed or timed out. Both need the same three facts, which is why they
// share a type rather than the confirmed one being reused for a list of
// timeouts.
type killOutcome struct {
	hash  uint32
	flush *flushCtx // #848: per-user hook context, non-nil only on originator moves
	// originated carries killState.originated out of the lock, because who may
	// announce the end of a kill is decided per record, not per member (#1359).
	originated bool
}

func (s *Server) sweepKills(grace time.Duration) {
	now := time.Now()
	var timedOut []killOutcome
	var confirmed []killOutcome

	s.killMu.Lock()
	for hash, st := range s.killing {
		switch {
		case now.After(st.deadline):
			timedOut = append(timedOut, killOutcome{hash: hash, originated: st.originated})
			delete(s.killing, hash)
		case !st.zeroSince.IsZero() && now.Sub(st.zeroSince) >= grace:
			confirmed = append(confirmed, killOutcome{hash: hash, flush: st.flush, originated: st.originated})
			delete(s.killing, hash)
		}
	}
	s.killMu.Unlock()

	for _, tk := range timedOut {
		slog.Warn("director: kill not confirmed within timeout, releasing LOOKUP hold (fallthrough)",
			"hash", tk.hash, "originated", tk.originated)
		// Announced only by the member that started the kill -- including here.
		// A peer's fallthrough is its own patience running out, not evidence
		// about anyone else's sessions, and broadcasting it would clear a hold
		// on a member still waiting for its own grace.
		if tk.originated {
			s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", tk.hash))
		}
		s.evacKillDone(tk.hash) // a timed-out move still frees its evacuation slot (#849)
	}
	for _, ck := range confirmed {
		slog.Info("director: kill confirmed, releasing LOOKUP hold",
			"hash", ck.hash, "originated", ck.originated)
		if ck.originated {
			// The originator's confirmation is authoritative -- it started the
			// move and saw the sessions go -- so peers may drop their holds at
			// once instead of waiting out their own grace.
			s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", ck.hash))
		}
		s.evacKillDone(ck.hash) // #849: confirmed move frees its slot; pull the next user
		if ck.flush != nil {
			// #848: old sessions are now gone — run the operator cleanup hook.
			// Async + best-effort; never gates the ring.
			s.runFlushHook(ck.hash, *ck.flush)
		}
	}
}
