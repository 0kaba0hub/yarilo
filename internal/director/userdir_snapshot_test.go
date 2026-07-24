package director

import (
	"testing"
	"time"
)

// TestUserDir_JoinSnapshot_InheritedOnJoin verifies #772 PR-1 end-to-end: a
// director that pins a user, then a second director joins the ring, must
// inherit that sticky assignment from the join-time userDir snapshot —
// instead of starting empty and only converging via the deterministic hash.
func TestUserDir_JoinSnapshot_InheritedOnJoin(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)

	// A pins a user to a specific backend (as a normal LOOKUP would).
	h := srvA.userDir.Set("pinned@example.com", "10.9.9.9:993", false)

	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 2)
	waitFor(t, 3*time.Second, func() bool {
		return len(srvB.membership.Members()) == 2
	})

	// B must have learned the pin via the snapshot on its join to A.
	waitFor(t, 3*time.Second, func() bool {
		e := srvB.userDir.GetByHash(h)
		return e != nil && e.Host == "10.9.9.9:993"
	})
	e := srvB.userDir.GetByHash(h)
	if e == nil || e.AssignBy != srvA.userDir.self {
		t.Fatalf("B's inherited entry must carry A's assign stamp, got %+v", e)
	}
}

// TestUserDir_JoinSnapshot_NewerLocalNotClobbered verifies the merge is
// ordered, not a blind overwrite: if the joiner already holds a strictly
// newer assignment for the same user, the older snapshot entry must not win.
func TestUserDir_JoinSnapshot_NewerLocalNotClobbered(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)
	h := srvA.userDir.Set("u@example.com", "10.0.0.1:993", false) // A: seq 1

	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 2)
	// B makes its own, causally-later assignment for the same user BEFORE
	// merging A's snapshot would matter — give B a high Lamport seq.
	srvB.userDir.observe(1000)
	srvB.userDir.Set("u@example.com", "10.0.0.2:993", false) // B: seq 1001

	waitFor(t, 3*time.Second, func() bool {
		return len(srvB.membership.Members()) == 2
	})
	// Let any snapshot/anti-entropy exchange settle.
	time.Sleep(300 * time.Millisecond)

	if e := srvB.userDir.GetByHash(h); e == nil || e.Host != "10.0.0.2:993" {
		t.Fatalf("B's newer local assignment must survive the snapshot merge, got %+v", e)
	}
}
