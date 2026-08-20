package director

import (
	"fmt"
	"log/slog"
	"time"
)

// evacuation is the resumable state of a graceful backend drain (#849). It holds a
// cursor (pending) over the users that had active sessions on the evacuating backend
// and the set of moves currently in flight (inflight), so the drain pauses at the
// throttle ceiling and resumes as each move's kill confirms. One drain per backend IP.
type evacuation struct {
	ip       string
	tag      string
	pending  []string        // usernames still to drain
	inflight map[uint32]bool // hashes whose confirmed-kill is in progress (the throttle window)
	max      int             // concurrency ceiling; 0 = unlimited
	moved    int             // total users drained, for the completion log
}

// startEvacuation begins (or refuses to duplicate) a graceful drain of backendIP:
// it marks the host down ring-wide and clears its pins via the existing flush event
// (down + DeleteByBackend on every replica, NO kick — the kicks are what we throttle),
// then drains the users that hold active sessions on it in a self-clocked window of
// maxParallel confirmed-kills. Returns the number of users queued.
//
// The re-login is deterministic without a proactive pin move: the host is now excluded
// from the ring, so a kicked user's re-LOOKUP rehashes to the same surviving backend it
// would have been moved to — and the confirmed-kill hold (#847) makes that re-LOOKUP
// wait until the old session is gone, closing the split-writer window. So a graceful
// drain is exactly a throttled, hold-gated version of the mass kick that --force does.
func (s *Server) startEvacuation(backendIP string, maxParallel int) int {
	tag := s.backendTag(backendIP)
	// Down + clear pins ring-wide; sessions untouched (we throttle their kicks below).
	s.ring.SetUp(backendIP, false, time.Now().Unix())
	s.userDir.DeleteByBackend(backendIP)
	s.originateRingEvent("RING-CHANGE", fmt.Sprintf("%s\tflush\t%s", backendIP, tag), nil)

	// One username per distinct routing hash — two sessions of the same user, or two
	// spellings that fold to one hash, drain as a single move.
	s.sessRecMu.RLock()
	byHash := make(map[uint32]string)
	for id := range s.sessByBE[backendIP] {
		if rec, ok := s.sessById[id]; ok {
			byHash[HashUsername(rec.user, s.hf)] = rec.user
		}
	}
	s.sessRecMu.RUnlock()

	pending := make([]string, 0, len(byHash))
	for _, user := range byHash {
		pending = append(pending, user)
	}

	e := &evacuation{
		ip:       backendIP,
		tag:      tag,
		pending:  pending,
		inflight: make(map[uint32]bool),
		max:      maxParallel,
	}

	s.evacMu.Lock()
	s.evac[backendIP] = e
	queued := len(pending)
	s.evacPumpLocked(e)
	empty := len(e.pending) == 0 && len(e.inflight) == 0
	if empty {
		delete(s.evac, backendIP)
	}
	s.evacMu.Unlock()

	slog.Info("director: graceful evacuation started", "ip", backendIP, "tag", tag,
		"users", queued, "max_parallel", maxParallel)
	return queued
}

// evacPumpLocked pulls users into the throttle window until it is full or the queue is
// drained. Caller holds evacMu.
func (s *Server) evacPumpLocked(e *evacuation) {
	for (e.max == 0 || len(e.inflight) < e.max) && len(e.pending) > 0 {
		user := e.pending[len(e.pending)-1]
		e.pending = e.pending[:len(e.pending)-1]
		hash := HashUsername(user, s.hf)
		if e.inflight[hash] {
			continue // already draining this hash (deduped, but be safe)
		}
		e.inflight[hash] = true
		e.moved++
		// Hold LOOKUP for this hash and kick its sessions off the evacuating host
		// only (conditional USER-KICKED with the old-backend field) — the same
		// primitives moveUser uses, so re-login lands on the rehash target once the
		// kill confirms.
		s.startKilling(hash)
		// #848: attach the flush-hook context so the operator cleanup runs once this
		// user's old sessions confirm gone. new = the rehash target the held re-LOOKUP
		// will land on (host already excluded from the ring); "" if none survive.
		newBackend := ""
		if b := s.ring.LookupBackendByTag(user, e.tag); b != nil {
			newBackend = b.IP
		}
		s.attachFlush(hash, flushCtx{user: user, oldBackend: e.ip, newBackend: newBackend})
		s.originateUserKick(user, e.ip, nil)
	}
}

// evacKillDone is the resume hook: sweepKills calls it for every hash whose kill has
// confirmed (or timed out). If that hash belongs to an active drain, its slot frees and
// the next user is pulled in; when the window empties and the queue is drained, the
// evacuation completes.
func (s *Server) evacKillDone(hash uint32) {
	s.evacMu.Lock()
	defer s.evacMu.Unlock()
	for ip, e := range s.evac {
		if !e.inflight[hash] {
			continue
		}
		delete(e.inflight, hash)
		s.evacPumpLocked(e)
		if len(e.pending) == 0 && len(e.inflight) == 0 {
			delete(s.evac, ip)
			slog.Info("director: graceful evacuation complete", "ip", ip, "tag", e.tag, "users_drained", e.moved)
		}
		return
	}
}
