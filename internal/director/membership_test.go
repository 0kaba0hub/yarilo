package director

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// startRingNode starts a director with ring membership active: secret
// authenticates JOINs it receives, seeds is who it tries to join via (nil =
// starts as a founder). Uses the same ctx for the listener and membership so
// t.Cleanup tears down both.
func startRingNode(t *testing.T, secret string, seeds []string, minMembers int) (*Server, string) {
	t.Helper()
	srv := NewWithOptions(Options{
		PingInterval: 24 * time.Hour, // disable client-facing PING during tests
		RingSecret:   []byte(secret),
		MinMembers:   minMembers,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	srv.opts.LocalIP = host
	srv.opts.LocalPort = port
	srv.membership.self = Member{IP: host, Port: port}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ln.Close()
	})
	go func() { _ = srv.listenOn(ctx, ln) }()
	srv.StartMembership(ctx, seeds)
	return srv, addr
}

// startKillableRingNode is startRingNode but also returns a kill func that
// simulates a real pod kill mid-test: cancels this node's own ctx (stopping
// its dial/accept loops immediately, not just at t.Cleanup) and closes both
// its listener and every already-established connection (both inbound
// accepted clients and this node's own outbound ring dial) — closing the
// listener alone leaves existing sockets open, unlike a real process kill,
// which severs everything immediately (#754).
func startKillableRingNode(t *testing.T, secret string, seeds []string, minMembers int) (srv *Server, addr string, kill func()) {
	t.Helper()
	srv = NewWithOptions(Options{
		PingInterval: 24 * time.Hour,
		RingSecret:   []byte(secret),
		MinMembers:   minMembers,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	srv.opts.LocalIP, srv.opts.LocalPort = host, port
	srv.membership.self = Member{IP: host, Port: port}

	ctx, cancel := context.WithCancel(context.Background())
	killed := false
	kill = func() {
		if killed {
			return
		}
		killed = true
		cancel()
		ln.Close()
		srv.clientMu.RLock()
		for c := range srv.clients {
			c.conn.Close()
		}
		srv.clientMu.RUnlock()
		srv.membership.rightMu.Lock()
		if srv.membership.dialConn != nil {
			srv.membership.dialConn.Close()
		}
		if srv.membership.passiveConn != nil {
			srv.membership.passiveConn.Close()
		}
		srv.membership.rightMu.Unlock()
	}
	t.Cleanup(kill)
	go func() { _ = srv.listenOn(ctx, ln) }()
	srv.StartMembership(ctx, seeds)
	return srv, addr, kill
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// ---- N=1: no peer machinery at all -----------------------------------------

func TestMembership_N1_NoPeerMachinery(t *testing.T) {
	srv, _ := startRingNode(t, "s3cret", nil, 3)

	members := srv.membership.Members()
	if len(members) != 1 || !members[0].equal(srv.membership.self) {
		t.Fatalf("N=1: Members() = %v, want [self]", members)
	}
	if _, ok := srv.membership.rightNeighbor(); ok {
		t.Error("N=1: rightNeighbor() must report no target")
	}
	srv.membership.rightMu.Lock()
	dialConn := srv.membership.dialConn
	passiveConn := srv.membership.passiveConn
	target := srv.membership.rightTarget
	srv.membership.rightMu.Unlock()
	if dialConn != nil || passiveConn != nil || !target.isZero() {
		t.Errorf("N=1: no dial/connection should ever be attempted, got dialConn=%v passiveConn=%v target=%v", dialConn, passiveConn, target)
	}

	// A singleton must still serve LOOKUP normally — the degradation ladder
	// promise: N=1 is today's ordinary single-replica mode, unchanged.
	srv.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true})
	if b := srv.ring.LookupBackend("user@example.com"); b == nil || b.IP != "10.0.0.1" {
		t.Errorf("N=1: LOOKUP must still work, got %+v", b)
	}
}

// ---- JOIN: HMAC accept / reject --------------------------------------------

func TestMembership_Join_AcceptsWithCorrectSecret(t *testing.T) {
	_, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)

	waitFor(t, 3*time.Second, func() bool {
		return len(srvB.membership.Members()) == 2
	})
}

func TestMembership_Join_RejectsWithWrongSecret(t *testing.T) {
	_, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "wrong-secret", []string{addrA}, 3)

	// Give the (doomed) join attempt a real chance, then confirm it never
	// succeeds — B must stay a singleton forever with a rejected secret.
	time.Sleep(500 * time.Millisecond)
	if members := srvB.membership.Members(); len(members) != 1 {
		t.Fatalf("join with wrong secret must not succeed, got members=%v", members)
	}
}

func TestMembership_Join_RejectsWhenNoSecretConfigured(t *testing.T) {
	_, addrA := startRingNode(t, "", nil, 3) // acceptor has ring auth disabled
	srvB, _ := startRingNode(t, "anything", []string{addrA}, 3)

	time.Sleep(500 * time.Millisecond)
	if members := srvB.membership.Members(); len(members) != 1 {
		t.Fatalf("join against a node with no ring_secret must not succeed, got members=%v", members)
	}
}

// ---- N=2: tie-break (only the lower member dials) + origin-absorb ---------

func TestMembership_N2_OnlyLowerMemberDials(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)

	waitFor(t, 3*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvB.membership.Members()) == 2
	})

	lower, higher := srvA, srvB
	if srvB.membership.self.less(srvA.membership.self) {
		lower, higher = srvB, srvA
	}

	waitFor(t, 3*time.Second, func() bool {
		lower.membership.rightMu.Lock()
		defer lower.membership.rightMu.Unlock()
		return lower.membership.dialConn != nil
	})

	// The higher-sorted member must never dial: it has no rightTarget of its
	// own (rightNeighbor() reports !ok for it), though its passiveConn does
	// get populated once the lower member's connection arrives
	// (serveRingConn's N=2 special case) — that's the "one connection
	// serves both directions" requirement, not a dial from this side.
	if _, ok := higher.membership.rightNeighbor(); ok {
		t.Error("N=2: the higher-sorted member must not compute a dial target")
	}
	waitFor(t, 3*time.Second, func() bool {
		higher.membership.rightMu.Lock()
		defer higher.membership.rightMu.Unlock()
		return higher.membership.passiveConn != nil
	})
	higher.membership.rightMu.Lock()
	higherDialConn := higher.membership.dialConn
	higher.membership.rightMu.Unlock()
	if higherDialConn != nil {
		t.Error("N=2: the higher-sorted member must never have a dialConn of its own")
	}
}

func TestMembership_N2_OriginAbsorb_RingChangeConverges(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)

	waitFor(t, 3*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvB.membership.Members()) == 2
	})
	// Let both sides' right-connections come up before generating traffic.
	time.Sleep(300 * time.Millisecond)

	// USER-MOVED (unlike RING-CHANGE's "up" case, which needs the backend
	// already known from the initial handshake — see applyRingChangeFields)
	// carries everything needed to apply on the other side from nothing,
	// making it the cleaner probe for pure ring-forwarding behavior.
	srvA.originateRingEvent("USER-MOVED", "user@example.com\t10.0.0.5\t993", nil)

	waitFor(t, 3*time.Second, func() bool {
		e := srvB.userDir.Get("user@example.com")
		return e != nil && e.Host == "10.0.0.5:993"
	})

	// Origin-absorb must stop the single round trip here: A's own lastSeq
	// entry for itself was set once at origination and must not keep
	// climbing from a bounce-back it then re-forwards.
	key := srvA.membership.self.IP + ":" + strconv.Itoa(srvA.membership.self.Port)
	srvA.membership.mu.RLock()
	seqAfterFirstRound := srvA.membership.lastSeq[key]
	srvA.membership.mu.RUnlock()

	time.Sleep(300 * time.Millisecond)

	srvA.membership.mu.RLock()
	seqLater := srvA.membership.lastSeq[key]
	srvA.membership.mu.RUnlock()
	if seqAfterFirstRound != seqLater {
		t.Errorf("event kept circulating after origin-absorb should have stopped it: %d -> %d", seqAfterFirstRound, seqLater)
	}
}

// ---- N=3: event reaches every member, more than one hop --------------------

func TestMembership_N3_EventReachesAllMembers(t *testing.T) {
	srvA, addrA := startRingNode(t, "shared-secret", nil, 3)
	srvB, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)
	srvC, _ := startRingNode(t, "shared-secret", []string{addrA}, 3)

	waitFor(t, 5*time.Second, func() bool {
		return len(srvA.membership.Members()) == 3 &&
			len(srvB.membership.Members()) == 3 &&
			len(srvC.membership.Members()) == 3
	})
	time.Sleep(300 * time.Millisecond) // let all right-neighbor dials settle

	srvA.originateRingEvent("USER-MOVED", "user@example.com\t10.0.0.7\t993", nil)

	waitFor(t, 5*time.Second, func() bool {
		eB := srvB.userDir.Get("user@example.com")
		eC := srvC.userDir.Get("user@example.com")
		return eB != nil && eB.Host == "10.0.0.7:993" && eC != nil && eC.Host == "10.0.0.7:993"
	})
}

// ---- #754 regression: killing a member must converge, not corrupt -------

// TestMembership_N3_KillMember_ConvergesWithoutCorruption reproduces the
// live-sandbox failure from #754: kill one member of a 3-member ring and
// verify (a) every survivor converges to exactly the same 2-member set,
// (b) that convergence is stable (no further flapping/mis-eviction), and
// (c) a pod that rejoins afterward never learns about the dead member.
func TestMembership_N3_KillMember_ConvergesWithoutCorruption(t *testing.T) {
	srvA, addrA, _ := startKillableRingNode(t, "shared-secret", nil, 3)
	srvB, addrB, killB := startKillableRingNode(t, "shared-secret", []string{addrA}, 3)
	srvC, _, _ := startKillableRingNode(t, "shared-secret", []string{addrA}, 3)

	waitFor(t, 10*time.Second, func() bool {
		return len(srvA.membership.Members()) == 3 &&
			len(srvB.membership.Members()) == 3 &&
			len(srvC.membership.Members()) == 3
	})
	time.Sleep(500 * time.Millisecond) // let all right-neighbor dials settle

	killB()

	// Death detection needs 3 failed dial attempts at ~1s spacing before
	// declaring dead, plus reconnect+handshake time for the retarget — on
	// a loaded/shared CI runner this can run well past what's comfortable
	// on a dev machine, so the window here is deliberately generous.
	survivors := []*Server{srvA, srvC}
	for _, s := range survivors {
		waitFor(t, 20*time.Second, func() bool {
			members := s.membership.Members()
			if len(members) != 2 {
				return false
			}
			for _, mem := range members {
				if mem.String() == addrB {
					return false // the dead member must be gone, not just uncounted
				}
			}
			return true
		})
	}

	// Stability: once converged, it must STAY converged — no further
	// eviction (the #754 bug specifically mis-declared the live wrap
	// neighbor dead one detection cycle after the real death).
	time.Sleep(4 * time.Second)
	for _, s := range survivors {
		members := s.membership.Members()
		if len(members) != 2 {
			t.Errorf("member %s: expected steady 2-member set after convergence, got %v (flapped)", s.membership.self, members)
		}
		for _, mem := range members {
			if mem.String() == addrB {
				t.Errorf("member %s: dead member %s resurrected after convergence", s.membership.self, addrB)
			}
		}
	}

	// A rejoin (simulating the replacement pod, new IP) must not learn
	// about the dead member via either survivor's DIRECTOR-LIST snapshot.
	srvD, _, _ := startKillableRingNode(t, "shared-secret", []string{addrA}, 3)
	waitFor(t, 10*time.Second, func() bool {
		return len(srvD.membership.Members()) == 3
	})
	for _, mem := range srvD.membership.Members() {
		if mem.String() == addrB {
			t.Errorf("rejoining member learned about the dead member %s", addrB)
		}
	}
}
