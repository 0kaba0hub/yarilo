package director

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMembership_LeavingMemberNeverRejoins is #1352, the defect its
// self-diagnosis pointed at: the CI log read "graceful ring leave self=X"
// followed immediately by "ring join accepted joiner=X", and the peer that had
// just been told to drop X carried it forever after.
//
// The path is the liveness probe. Leave closes every ring connection, so the
// leaver's own left-connection handler fires -- and that handler probes with a
// full JOIN, which re-announces the leaver to the peer it probes.
//
// Driven through onLeftConnLost rather than asserted on joinVia alone, because
// the defect is in a caller reaching for a builder that announces membership as
// a side effect of asking a question.
func TestMembership_LeavingMemberNeverRejoins(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)
	srvL, _ := startRingNode(t, "shared-secret", []string{addrA}, 2)
	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvL.membership.Members()) == 2
	})

	srvL.membership.Leave()
	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 1
	})

	// The trigger the leave itself produces: the leaver's left connection is
	// gone, so it would probe to find out whether that neighbour died.
	srvL.membership.onLeftConnLost(context.Background(), srvA.membership.self)

	// The peer must stay at one. A JOIN from the leaver would put it back.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := srvA.membership.Members(); len(got) != 1 {
			t.Fatalf("the peer re-admitted the member that had just left: %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the refusal is stated where the join is built, so a future caller
	// reaching for it on the way out gets the same answer.
	if err := srvL.membership.joinVia(context.Background(), addrA); !errors.Is(err, errLeaving) {
		t.Errorf("joinVia after Leave = %v, want errLeaving", err)
	}
}

// TestMembership_LeavingMemberDeclaresNobodyDead: the same handler's other
// half. A member on its way out must not spend its last moments declaring a
// neighbour dead — it will not be there to correct the verdict, and the
// neighbour is usually alive.
func TestMembership_LeavingMemberDeclaresNobodyDead(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)
	srvL, _ := startRingNode(t, "shared-secret", []string{addrA}, 2)
	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvL.membership.Members()) == 2
	})

	srvL.membership.Leave()
	srvL.membership.onLeftConnLost(context.Background(), srvA.membership.self)

	// Asserted on the LEAVER's own view: the verdict it would reach is
	// "unreachable, declaring dead" about a neighbour that is fine, and the
	// DIRECTOR-REMOVE that follows goes out to the ring it is abandoning.
	for _, mem := range srvL.membership.Members() {
		if mem.equal(srvA.membership.self) {
			return
		}
	}
	t.Fatalf("the leaver declared its live neighbour dead on the way out: %v", srvL.membership.Members())
}
