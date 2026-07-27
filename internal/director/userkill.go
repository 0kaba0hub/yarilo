package director

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Confirmed ring-wide kick (#847), the yarilo analogue of Dovecot's
// user_kill_state machine adapted to our session-registry + login-proxy model
// (no auth-IPC). When a user is kicked or moved, the director marks the user
// "killing" and holds LOOKUP for it, so a concurrent login cannot be assigned a
// fresh backend while the old sessions are still being torn down elsewhere — the
// split-writer window the co-located single-writer model (#788) exists to close.
// The kill is confirmed complete when the user's RING-WIDE session count (the
// #804 replicated registry) has stayed at zero for the confirm grace, or the
// hard timeout elapses (fallthrough, so a stuck holder never locks a user out).
//
// killReason=killing is returned to the login proxy as a RETRYABLE lookup
// failure — the proxy re-LOOKUPs (bounded) rather than erroring the client, so a
// kick does not turn concurrent logins into user-visible errors.

// killState is one user's kill-in-progress record. deadline is computed LOCALLY
// on each director from a replicated DURATION (never a wall-clock deadline sent
// on the wire — pod-clock skew would make it unstable, the #772 lesson).
// zeroSince is when the ring-wide session count was first observed at zero for
// this kill (zero value = not yet observed at zero); the confirm requires it to
// hold for userKillConfirmGrace so an in-flight SESSION-OPEN dipping the count
// momentarily cannot trigger a premature clear.
type killState struct {
	deadline  time.Time
	zeroSince time.Time
}

// killReason is the retryable LOOKUP failure reason a held user gets.
const killReason = "killing"

// startKilling marks user (by hash) killing locally and replicates it ring-wide
// as a DURATION, so every director gates LOOKUP for the same user. Called at the
// start of a kick / move-kill, before the sessions are torn down.
func (s *Server) startKilling(hash uint32) {
	ttl := s.opts.userKillTimeout()
	s.killMu.Lock()
	s.killing[hash] = killState{deadline: time.Now().Add(ttl)}
	s.killMu.Unlock()
	// Replicate the TTL in milliseconds; receivers compute their own deadline.
	s.membership.originate("USER-KILLING", fmt.Sprintf("%d\t%d", hash, ttl.Milliseconds()))
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
	hash := HashUsername(user, s.opts.usernameHashLowercase())
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
	hash := HashUsername(user, s.opts.usernameHashLowercase())
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
	// Only arm on the transition to zero; don't push the timestamp forward on
	// every subsequent close (there are none once at zero, but be safe).
	if st, ok = s.killing[hash]; ok && st.zeroSince.IsZero() {
		st.zeroSince = time.Now()
		s.killing[hash] = st
	}
	s.killMu.Unlock()
}

// userSessionCount counts this director's ring-wide view of active sessions for
// the user hash (local + #804 remote replicas).
func (s *Server) userSessionCount(hash uint32) int {
	lc := s.opts.usernameHashLowercase()
	s.sessRecMu.RLock()
	defer s.sessRecMu.RUnlock()
	n := 0
	for _, rec := range s.sessById {
		if HashUsername(rec.user, lc) == hash {
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

func (s *Server) sweepKills(grace time.Duration) {
	now := time.Now()
	var confirmed, timedOut []uint32

	s.killMu.Lock()
	for hash, st := range s.killing {
		switch {
		case now.After(st.deadline):
			timedOut = append(timedOut, hash)
			delete(s.killing, hash)
		case !st.zeroSince.IsZero() && now.Sub(st.zeroSince) >= grace:
			confirmed = append(confirmed, hash)
			delete(s.killing, hash)
		}
	}
	s.killMu.Unlock()

	for _, hash := range timedOut {
		slog.Warn("director: kill not confirmed within timeout, releasing LOOKUP hold (fallthrough)", "hash", hash)
		s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", hash))
	}
	for _, hash := range confirmed {
		slog.Info("director: kill confirmed ring-wide, releasing LOOKUP hold", "hash", hash)
		s.membership.originate("USER-KILL-DONE", fmt.Sprintf("%d", hash))
	}
}
