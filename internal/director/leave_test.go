package director

import (
	"context"
	"testing"
	"time"
)

// TestMembership_GracefulLeave_EvictsImmediately verifies #770: a director
// calling Leave() originates DIRECTOR-REMOVE for itself so peers drop it
// at once — no death-detection window — and stops answering JOINs.
func TestMembership_GracefulLeave_EvictsImmediately(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)
	srvC, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)
	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 3 &&
			len(srvB.membership.Members()) == 3 &&
			len(srvC.membership.Members()) == 3
	})
	// Member-COUNT convergence does not imply the ring DIALS are all
	// established — reconcile may still be connecting right after formation.
	// Leave() broadcasts DIRECTOR-REMOVE over the CURRENT ring connections,
	// so firing it before B's own dial completes routes the removal the
	// long way and was the source of the occasional >2s flake. Wait for the
	// ring to be fully wired deterministically instead of guessing a sleep
	// (a fixed sleep still flaked under CI load): in a healthy N=3 cycle each
	// node holds exactly two ring connections (one dial-out to its right, one
	// accepted from its left). Once all three reach that, eviction is
	// sub-second (direct envelope propagation, no timer/retry).
	ringConnCount := func(s *Server) int {
		s.membership.rightMu.Lock()
		defer s.membership.rightMu.Unlock()
		return len(s.membership.ringConns)
	}
	waitFor(t, 5*time.Second, func() bool {
		return ringConnCount(srvA) >= 2 && ringConnCount(srvB) >= 2 && ringConnCount(srvC) >= 2
	})

	leaver := srvB.membership.self
	srvB.membership.Leave()

	// A and C must drop B fast -- via the propagated DIRECTOR-REMOVE, not by
	// waiting out the neighbor-probe window (a generous but sub-probe-window
	// bound; the probe path alone would take several seconds of retries).
	//
	// The assertion stays at 2s, but a failure keeps watching to 10s and says
	// which of two things happened, because re-running cannot tell them apart
	// and a re-run is what has happened three times (#1352):
	//
	//   - it converged late -> the budget is too tight for a loaded machine,
	//     and the number says by how much;
	//   - it never converged -> graceful leave genuinely does not always
	//     propagate, which would show up in a rolling restart of the director.
	const budget = 2 * time.Second
	const patience = 10 * time.Second
	for _, s := range []*Server{srvA, srvC} {
		dropped, took := waitForDrop(s, leaver, patience)
		switch {
		case !dropped:
			t.Fatalf("member %s never dropped the gracefully-left %s within %s: %v -- "+
				"this is propagation, not a slow machine",
				s.membership.self, leaver, patience, s.membership.Members())
		case took > budget:
			t.Fatalf("member %s dropped the gracefully-left %s only after %s (budget %s) -- "+
				"it propagates, but not within the window this test asserts",
				s.membership.self, leaver, took.Round(time.Millisecond), budget)
		}
	}
}

// TestMembership_GracefulLeave_RejectsJoin verifies a leaving director
// answers new JOINs with JOIN-FAIL, so no fresh joiner can learn it (#765
// phantom-injection source closed on the planned-exit path).
func TestMembership_GracefulLeave_RejectsJoin(t *testing.T) {
	srv, addr := startRingNode(t, "shared-secret", nil, 3)
	srv.membership.Leave()

	// A fresh joiner dialing the leaving node must be rejected.
	srvJ, _ := startRingNode(t, "shared-secret", nil, 3)
	if err := srvJ.membership.joinVia(context.Background(), addr); err == nil {
		t.Fatal("join against a leaving director must fail")
	}
}

// waitForDrop watches one member until it stops listing target, and reports how
// long that took. Separated from the assertion so a failure can say whether the
// state arrived late or not at all -- the distinction a re-run destroys.
func waitForDrop(s *Server, target Member, patience time.Duration) (bool, time.Duration) {
	start := time.Now()
	deadline := start.Add(patience)
	for {
		has := false
		for _, m := range s.membership.Members() {
			if m.equal(target) {
				has = true
				break
			}
		}
		if !has {
			return true, time.Since(start)
		}
		if time.Now().After(deadline) {
			return false, time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
