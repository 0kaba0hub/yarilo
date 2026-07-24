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
	// long way and was the source of the occasional >2s flake. Let the ring
	// connections settle first; eviction is then sub-second (direct
	// envelope propagation, no timer/retry).
	time.Sleep(300 * time.Millisecond)

	leaver := srvB.membership.self
	srvB.membership.Leave()

	// A and C must drop B fast — via the propagated DIRECTOR-REMOVE, not by
	// waiting out the neighbor-probe window (a generous but sub-probe-window
	// bound; the probe path alone would take several seconds of retries).
	for _, s := range []*Server{srvA, srvC} {
		ok := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			has := false
			for _, m := range s.membership.Members() {
				if m.equal(leaver) {
					has = true
					break
				}
			}
			if !has {
				ok = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("member %s still holds the gracefully-left %s after 2s: %v",
				s.membership.self, leaver, s.membership.Members())
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
