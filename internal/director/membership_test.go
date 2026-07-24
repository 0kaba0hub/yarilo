package director

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// startRingNode starts a director with ring membership active: secret
// authenticates JOINs it receives, seeds is who it tries to join via (nil =
// starts as a founder). Uses the same ctx for the listener and membership so
// t.Cleanup tears down both. Anti-entropy is disabled so tests that assert
// on exact propagation paths stay strict — the periodic snapshot would
// otherwise eventually paper over a broken direct broadcast; use
// startRingNodeAE to enable it explicitly.
func startRingNode(t *testing.T, secret string, seeds []string, minMembers int) (*Server, string) {
	t.Helper()
	return startRingNodeAE(t, secret, seeds, minMembers, -1)
}

func startRingNodeAE(t *testing.T, secret string, seeds []string, minMembers int, antiEntropy time.Duration) (*Server, string) {
	t.Helper()
	srv := NewWithOptions(Options{
		PingInterval:        24 * time.Hour, // disable client-facing PING during tests
		RingSecret:          []byte(secret),
		MinMembers:          minMembers,
		AntiEntropyInterval: antiEntropy,
		SeedPollInterval:    -1, // one-shot join — periodic seed re-poll would mask direct-path regressions, same as anti-entropy
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
		PingInterval:        24 * time.Hour,
		RingSecret:          []byte(secret),
		MinMembers:          minMembers,
		AntiEntropyInterval: -1, // see startRingNode — direct-path tests stay strict
		SeedPollInterval:    -1,
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
		for c := range srv.membership.ringConns {
			c.Close()
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
	ringConns := len(srv.membership.ringConns)
	target := srv.membership.rightTarget
	srv.membership.rightMu.Unlock()
	if dialConn != nil || ringConns != 0 || !target.isZero() {
		t.Errorf("N=1: no dial/connection should ever be attempted, got dialConn=%v ringConns=%d target=%v", dialConn, ringConns, target)
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

// TestMembership_Join_RejectsSelfDial reproduces #759's root cause: a
// load-balanced ClusterIP seed can route a pod's own JOIN dial back to
// itself. Before handleJoin rejected this, it looked like a normal,
// immediate success (addMember(self) is a harmless no-op) — but joinLoop
// then stops retrying the seed forever, leaving the pod stuck as a
// permanently isolated N=1 that never discovers any real peer.
func TestMembership_Join_RejectsSelfDial(t *testing.T) {
	srv, addr := startRingNode(t, "shared-secret", nil, 3)

	err := srv.membership.joinVia(context.Background(), addr)
	if err == nil {
		t.Fatal("a JOIN that dials back to self must be rejected, got nil error")
	}
	// joinLoop keys its fast-retry-vs-backoff decision on this exact
	// classification (#759): a self-dial must NOT look like an ordinary
	// rejection, or every (expected, ~1/N) self-dial walks the exponential
	// backoff and stretches formation into minutes.
	if !errors.Is(err, errJoinSelfDial) {
		t.Fatalf("self-dial rejection must classify as errJoinSelfDial, got: %v", err)
	}
	if members := srv.membership.Members(); len(members) != 1 {
		t.Fatalf("a rejected self-dial JOIN must not change membership, got %v", members)
	}
}

// TestMembership_AntiEntropy_HealsMissedMemberBroadcast verifies the #759
// safety net: if one node's member view gains an entry whose DIRECTOR-ADD
// broadcast a directly-connected peer never received (simulated by adding
// the member out-of-band, with no broadcast at all), the periodic
// DIRECTOR-LIST snapshot re-broadcast must deliver it within a few
// intervals — direct broadcasts are the fast path, anti-entropy is the
// bounded backstop for exactly the concurrent-formation races that
// dropped them live.
func TestMembership_AntiEntropy_HealsMissedMemberBroadcast(t *testing.T) {
	srvA, addrA := startRingNodeAE(t, "shared-secret", nil, 3, 200*time.Millisecond)
	srvB, _ := startRingNodeAE(t, "shared-secret", []string{addrA}, 3, 200*time.Millisecond)

	waitFor(t, 3*time.Second, func() bool {
		return len(srvA.membership.Members()) == 2 && len(srvB.membership.Members()) == 2
	})
	time.Sleep(300 * time.Millisecond) // let the N=2 ring connection settle

	// A real, dialable third node (an unjoined singleton) — using a live
	// address keeps reconcile()'s post-merge dial from declaring it dead
	// mid-test, which would turn this into a death-path test instead.
	srvD, _ := startRingNode(t, "shared-secret", nil, 3)

	// Simulate the missed broadcast: A learns about D, no DIRECTOR-ADD is
	// ever sent. Without anti-entropy, B would now be stuck at 2 members
	// forever (this is live #759 problem 2's end state).
	srvA.membership.addMember(srvD.membership.self)
	srvA.membership.reconcile()

	waitFor(t, 3*time.Second, func() bool {
		for _, mem := range srvB.membership.Members() {
			if mem.equal(srvD.membership.self) {
				return true
			}
		}
		return false
	})
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
	// own (rightNeighbor() reports !ok for it), though the connection the
	// lower member dials in does get registered in ringConns once it
	// arrives — that's the "one connection serves both directions"
	// requirement, not a dial from this side.
	if _, ok := higher.membership.rightNeighbor(); ok {
		t.Error("N=2: the higher-sorted member must not compute a dial target")
	}
	waitFor(t, 3*time.Second, func() bool {
		higher.membership.rightMu.Lock()
		defer higher.membership.rightMu.Unlock()
		return len(higher.membership.ringConns) > 0
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

	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(srvA.membership.Members()) == 3 &&
			len(srvB.membership.Members()) == 3 &&
			len(srvC.membership.Members()) == 3 {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("initial 3-way convergence timed out; A=%v B=%v C=%v",
			srvA.membership.Members(), srvB.membership.Members(), srvC.membership.Members())
	}
	time.Sleep(500 * time.Millisecond) // let all right-neighbor dials settle

	killB()

	// Death detection needs 3 failed dial attempts at ~1s spacing before
	// declaring dead, plus reconnect+handshake time for the retarget — on
	// a loaded/shared CI runner this can run well past what's comfortable
	// on a dev machine, so the window here is deliberately generous.
	survivors := []*Server{srvA, srvC}
	for _, s := range survivors {
		var last []Member
		deadline := time.Now().Add(20 * time.Second)
		ok := false
		for time.Now().Before(deadline) {
			last = s.membership.Members()
			if len(last) == 2 {
				dead := false
				for _, mem := range last {
					if mem.String() == addrB {
						dead = true
						break
					}
				}
				if !dead {
					ok = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("member %s: did not converge to 2 members without %s within 20s; last observed members=%v", s.membership.self, addrB, last)
		}
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
	rejoinOK := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(srvD.membership.Members()) == 3 {
			rejoinOK = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !rejoinOK {
		t.Fatalf("rejoin convergence timed out; D=%v", srvD.membership.Members())
	}
	for _, mem := range srvD.membership.Members() {
		if mem.String() == addrB {
			t.Errorf("rejoining member learned about the dead member %s", addrB)
		}
	}
}

// TestMembership_N3_ShrinkToN2_ReusesExistingConnectionForward isolates the
// exact #754 follow-up scenario, independent of dial/accept timing luck:
// kill the wrap (highest-sorted) member of a 3-ring, so the surviving
// low→mid dial edge is completely untouched by the death — low was already
// dialing mid before the kill and keeps dialing the same target after, no
// redial involved. mid alone detects the death (its own dial to high fails)
// and must deliver DIRECTOR-REMOVE to low over that same still-open,
// pre-existing connection now that mid has become the N=2 passive member —
// there is no new connection anywhere in this path for a resync to hide
// behind. Under the old dialConn/passiveConn model this connection was
// classified (at accept time, while still N=3) as not the passive path and
// never revisited, so the event had nowhere to go and was silently dropped
// forever; broadcastRing has no such role to go stale.
func TestMembership_N3_ShrinkToN2_ReusesExistingConnectionForward(t *testing.T) {
	n1, addr1, kill1 := startKillableRingNode(t, "shared-secret", nil, 3)
	n2, addr2, kill2 := startKillableRingNode(t, "shared-secret", []string{addr1}, 3)
	n3, addr3, kill3 := startKillableRingNode(t, "shared-secret", []string{addr1}, 3)

	nodes := []*Server{n1, n2, n3}
	addrOf := map[*Server]string{n1: addr1, n2: addr2, n3: addr3}
	killOf := map[*Server]func(){n1: kill1, n2: kill2, n3: kill3}

	ready := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(n1.membership.Members()) == 3 && len(n2.membership.Members()) == 3 && len(n3.membership.Members()) == 3 {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("initial 3-way convergence timed out; n1=%v n2=%v n3=%v",
			n1.membership.Members(), n2.membership.Members(), n3.membership.Members())
	}
	time.Sleep(500 * time.Millisecond) // let all right-neighbor dials settle

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].membership.self.less(nodes[j].membership.self) })
	low, mid, high := nodes[0], nodes[1], nodes[2]

	killOf[high]()
	deadAddr := addrOf[high]

	// Tight on purpose: long enough for the direct one-hop forward under
	// test (death detection ~3 failed attempts at ~1s spacing, then an
	// immediate broadcastRing), but with no fresh connection anywhere in
	// this path for a resync to accidentally paper over a still-broken
	// forward — this window is what makes the test actually isolate the
	// fix rather than just eventually observe it.
	for _, s := range []*Server{low, mid} {
		var last []Member
		survivorDeadline := time.Now().Add(6 * time.Second)
		ok := false
		for time.Now().Before(survivorDeadline) {
			last = s.membership.Members()
			if len(last) == 2 {
				dead := false
				for _, mem := range last {
					if mem.String() == deadAddr {
						dead = true
						break
					}
				}
				if !dead {
					ok = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("member %s: did not receive DIRECTOR-REMOVE for wrap member %s over the pre-existing connection within 6s; last observed members=%v",
				s.membership.self, deadAddr, last)
		}
	}
}

// TestMembership_Formation_ViaLoadBalancedSeed is the in-process gate for
// #759's live failure mode: N members join SIMULTANEOUSLY through one
// shared seed address that — like a k8s ClusterIP behind kube-proxy —
// proxies every connection to a RANDOM member, including the dialer
// itself. Under that formation race the per-node right-neighbor dials are
// computed from divergent views and are not guaranteed to form one
// connected graph, so convergence must not depend on them: the periodic
// seed re-poll (every pod can always reach the seed, so it is a
// guaranteed crossing point) plus anti-entropy must converge everyone
// regardless of routing luck. Run with -count to shake the randomness.
func TestMembership_Formation_ViaLoadBalancedSeed(t *testing.T) {
	const n = 3

	var (
		mu    sync.Mutex
		addrs []string
	)
	seedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	t.Cleanup(func() { seedLn.Close() })
	go func() {
		for {
			c, aErr := seedLn.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				mu.Lock()
				var target string
				if len(addrs) > 0 {
					target = addrs[rand.IntN(len(addrs))]
				}
				mu.Unlock()
				if target == "" {
					return
				}
				b, dErr := net.Dial("tcp", target)
				if dErr != nil {
					return
				}
				defer b.Close()
				go func() { _, _ = io.Copy(b, c) }()
				_, _ = io.Copy(c, b)
			}(c)
		}
	}()
	seedAddr := seedLn.Addr().String()

	// Phase 1: bring up every node's listener (registering it with the
	// seed router) WITHOUT starting membership, so that when membership
	// does start, all N race their joins concurrently — the divergent
	// formation views are the whole point of this test.
	srvs := make([]*Server, 0, n)
	starts := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		srv := NewWithOptions(Options{
			PingInterval:        24 * time.Hour,
			RingSecret:          []byte("shared-secret"),
			MinMembers:          n,
			AntiEntropyInterval: 500 * time.Millisecond,
			SeedPollInterval:    300 * time.Millisecond,
		})
		ln, lErr := net.Listen("tcp", "127.0.0.1:0")
		if lErr != nil {
			t.Fatalf("listen: %v", lErr)
		}
		addr := ln.Addr().String()
		host, portStr, _ := net.SplitHostPort(addr)
		port, pErr := strconv.Atoi(portStr)
		if pErr != nil {
			t.Fatalf("parse port: %v", pErr)
		}
		srv.opts.LocalIP, srv.opts.LocalPort = host, port
		srv.membership.self = Member{IP: host, Port: port}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			ln.Close()
		})
		go func() { _ = srv.listenOn(ctx, ln) }()

		mu.Lock()
		addrs = append(addrs, addr)
		mu.Unlock()

		starts = append(starts, func() { srv.StartMembership(ctx, []string{seedAddr}) })
		srvs = append(srvs, srv)
	}

	// Phase 2: simultaneous start.
	for _, start := range starts {
		start()
	}

	deadline := time.Now().Add(15 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		all := true
		for _, srv := range srvs {
			if len(srv.membership.Members()) != n {
				all = false
				break
			}
		}
		if all {
			converged = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !converged {
		views := make([]string, 0, n)
		for _, srv := range srvs {
			views = append(views, formatMemberList(srv.membership.Members()))
		}
		t.Fatalf("formation via load-balanced seed did not converge to %d everywhere within 15s; views=%v", n, views)
	}
}

// TestMembership_Formation_ViaHeadlessDNSSeed covers the recommended
// production seed shape (#759): a hostname (in k8s the headless
// -director-ring Service) whose resolution returns EVERY member address.
// expandSeed must fan the poll out to all resolved addresses except self
// each cycle — deterministic convergence within about one poll interval,
// no routing randomness, no self-dials at all. This also pins the
// self-exclusion behavior that sidesteps Go's RFC 6724 own-IP-first
// destination ordering.
func TestMembership_Formation_ViaHeadlessDNSSeed(t *testing.T) {
	const n = 3

	var (
		mu    sync.Mutex
		addrs []string
	)
	resolve := func(ctx context.Context, host string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, addrs...), nil
	}

	srvs := make([]*Server, 0, n)
	starts := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		srv := NewWithOptions(Options{
			PingInterval:        24 * time.Hour,
			RingSecret:          []byte("shared-secret"),
			MinMembers:          n,
			AntiEntropyInterval: -1, // seed fan-out alone must converge this
			SeedPollInterval:    300 * time.Millisecond,
		})
		srv.membership.resolveHost = resolve
		ln, lErr := net.Listen("tcp", "127.0.0.1:0")
		if lErr != nil {
			t.Fatalf("listen: %v", lErr)
		}
		addr := ln.Addr().String()
		host, portStr, _ := net.SplitHostPort(addr)
		port, pErr := strconv.Atoi(portStr)
		if pErr != nil {
			t.Fatalf("parse port: %v", pErr)
		}
		srv.opts.LocalIP, srv.opts.LocalPort = host, port
		srv.membership.self = Member{IP: host, Port: port}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() {
			cancel()
			ln.Close()
		})
		go func() { _ = srv.listenOn(ctx, ln) }()

		mu.Lock()
		addrs = append(addrs, addr)
		mu.Unlock()

		starts = append(starts, func() { srv.StartMembership(ctx, []string{"ring.test:9102"}) })
		srvs = append(srvs, srv)
	}

	for _, start := range starts {
		start()
	}

	deadline := time.Now().Add(5 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		all := true
		for _, srv := range srvs {
			if len(srv.membership.Members()) != n {
				all = false
				break
			}
		}
		if all {
			converged = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !converged {
		views := make([]string, 0, n)
		for _, srv := range srvs {
			views = append(views, formatMemberList(srv.membership.Members()))
		}
		t.Fatalf("formation via headless-DNS seed did not converge to %d everywhere within 5s; views=%v", n, views)
	}
}
