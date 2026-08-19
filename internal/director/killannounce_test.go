package director

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Announcing the end of a kill belongs to the member that started it.
//
// Every member keeps its own record and sweeps it independently, so before this
// rule whichever sweep finished first broadcast USER-KILL-DONE and every other
// member deleted its record -- including the originator's, which carries the
// flush context. The hook then ran only when the originator won a three-way
// race, about one move in three, which is why the same experiment produced a
// clean confirm one day and a fallthrough the next (#1359).
func TestOnlyTheOriginatorAnnouncesTheEndOfAKill(t *testing.T) {
	const user = "u1@d.test"

	tests := []struct {
		name         string
		arm          func(s *Server, hash uint32)
		wantAnnounce bool
	}{
		{
			// A peer that confirms first must stay quiet: its record is not the
			// one that decides, and announcing would delete the originator's --
			// flush context and all.
			name: "a peer's confirmation announces nothing",
			arm: func(s *Server, hash uint32) {
				s.applyKilling(hash, time.Second)
				s.noteSessionClosed(user)
			},
			wantAnnounce: false,
		},
		{
			// The fallthrough is the same question with a worse answer: a peer's
			// patience running out is not evidence about anyone else's sessions,
			// and clearing a hold on a member still inside its own grace is the
			// harm it would do.
			name: "a peer's fallthrough announces nothing",
			arm: func(s *Server, hash uint32) {
				s.applyKilling(hash, time.Millisecond)
				time.Sleep(5 * time.Millisecond)
			},
			wantAnnounce: false,
		},
		{
			name: "the originator's confirmation announces",
			arm: func(s *Server, hash uint32) {
				s.startKilling(hash) // quiesced: no sessions, so it arms at once
			},
			wantAnnounce: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithOptions(Options{UserKillConfirmGrace: time.Millisecond, UserKillTimeout: time.Second})
			hash := HashUsername(user, s.hf)
			tc.arm(s, hash)
			time.Sleep(20 * time.Millisecond)

			// originate bumps the ring sequence once per event, which is what a
			// peer would receive. Measured across the sweep alone, so the
			// USER-KILLING that startKilling sends is not counted.
			before := s.membership.seq.Load()
			s.sweepKills(time.Millisecond)
			announced := s.membership.seq.Load() - before

			if (announced > 0) != tc.wantAnnounce {
				t.Errorf("the sweep originated %d ring events, want announce=%v", announced, tc.wantAnnounce)
			}
		})
	}
}

// The hook is the reason the rule exists: it must run on the originator every
// time, not when it happens to win. A peer confirming first -- the exact race
// that was losing it -- must change nothing.
func TestTheHookRunsEvenWhenAPeerConfirmsFirst(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	script := writeHookScript(t, out)
	const user = "u1@d.test"

	s := NewWithOptions(Options{
		FlushProgram:         script,
		FlushProgramTimeout:  2 * time.Minute,
		UserKillConfirmGrace: time.Millisecond,
		UserKillTimeout:      30 * time.Second,
	})
	s.userDir.Set(user, "10.0.0.1:993", false)
	s.moveUser(user, "10.0.0.2:993", nil)

	// A peer announces the end first -- which used to delete the originator's
	// record, flush context and all.
	s.applyKillDone(HashUsername(user, s.hf))

	time.Sleep(10 * time.Millisecond)
	s.sweepKills(time.Millisecond)

	if got := strings.TrimSpace(waitForFile(t, out)); got == "" {
		t.Fatal("the flush hook did not run after a peer announced the kill first -- " +
			"the originator's record was taken away with its flush context")
	}
}

// The peers' side of the trade, and both of its named costs. They no longer get
// an early release from another peer's sweep, so each must clear its own hold --
// by its own confirmation, or by its own deadline when the originator is gone.
// Neither path is silent, and both are bounded; a hold that only some other
// member could release would be neither.
func TestAPeerClearsItsOwnHold(t *testing.T) {
	const user = "u2@d.test"

	tests := []struct {
		name string
		// arm sets up the peer's record, and returns how long to wait before
		// the sweep that must clear it.
		arm  func(s *Server, hash uint32) time.Duration
		what string
	}{
		{
			name: "by its own confirmation, once the sessions are gone",
			arm: func(s *Server, hash uint32) time.Duration {
				s.applyKilling(hash, 10*time.Second)
				s.noteSessionClosed(user)
				return 20 * time.Millisecond
			},
			what: "the grace elapsed and the peer confirmed for itself",
		},
		{
			name: "by its own deadline, when the originator never speaks again",
			arm: func(s *Server, hash uint32) time.Duration {
				s.applyKilling(hash, 10*time.Millisecond)
				return 30 * time.Millisecond
			},
			what: "the replicated TTL ran out",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithOptions(Options{UserKillConfirmGrace: time.Millisecond, UserKillTimeout: time.Second})
			hash := HashUsername(user, s.hf)
			wait := tc.arm(s, hash)
			if !s.isKilling(hash) {
				t.Fatal("the peer is not holding LOOKUP at all; the case is not set up")
			}
			time.Sleep(wait)
			s.sweepKills(time.Millisecond)

			if s.isKilling(hash) {
				t.Errorf("the peer still holds LOOKUP although %s -- a hold only another member can clear is a stuck user", tc.what)
			}
		})
	}
}
