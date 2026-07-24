package director

import (
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// TestUserDir_LookupPropagatesToRing is the #772 PR-2 end-to-end: a fresh
// sticky assignment made by a normal LOOKUP on one director must reach the
// other ring members as a USER-ASSIGN event — so every director pins the
// user to the same backend without waiting for a rejoin snapshot.
func TestUserDir_LookupPropagatesToRing(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 2)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 2)
	waitFor(t, 3*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvB.membership.Members()) == 2
	})
	time.Sleep(200 * time.Millisecond) // let the ring connection settle

	// Backend on A so its LOOKUP resolves; B needs nothing to receive the pin.
	srvA.ring.AddBackend(&ring.Backend{IP: "10.7.7.7", Port: 993, Tag: "imap", Up: true})

	// Drive a real LOOKUP against A as a login client.
	conn, sc := dialTest(t, addrA)
	readHandshake(t, sc)
	sendHandshake(t, conn)
	if _, err := conn.Write([]byte("LOOKUP\t1\talice@example.com\timap\n")); err != nil {
		t.Fatalf("send LOOKUP: %v", err)
	}
	if line := readLine(t, sc); line[:4] != "HOST" {
		t.Fatalf("expected HOST, got %q", line)
	}

	want := "10.7.7.7:993"
	waitFor(t, 3*time.Second, func() bool {
		e := srvB.userDir.Get("alice@example.com")
		return e != nil && e.Host == want
	})
	e := srvB.userDir.Get("alice@example.com")
	if e == nil || e.Host != want {
		t.Fatalf("B did not receive the LOOKUP pin via USER-ASSIGN, got %+v", e)
	}
	if e.AssignBy != srvA.userDir.self {
		t.Fatalf("propagated entry must carry A's assign stamp, got by=%q", e.AssignBy)
	}
}
