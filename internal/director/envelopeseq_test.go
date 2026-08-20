package director

import (
	"fmt"
	"testing"
	"time"
)

// Dedup has to mean "I have seen THIS event", not "I have seen something
// newer". The ring is redundant on purpose -- every member is reachable by two
// paths -- and the backup copy of an event always arrives after the direct copy
// of the next one. A high-water mark therefore discarded exactly the copy the
// redundancy exists to provide, and did it before forwarding, so the loss
// spread to everyone downstream (#1359).
func TestSeenSeqsAdmitsByIdentityNotRecency(t *testing.T) {
	tests := []struct {
		name  string
		order []uint64
		want  []bool
	}{
		{
			name:  "the late copy of an earlier event is still applied",
			order: []uint64{6, 5},
			want:  []bool{true, true},
		},
		{
			name:  "a true duplicate is refused",
			order: []uint64{5, 6, 5},
			want:  []bool{true, true, false},
		},
		{
			name:  "in-order delivery is unchanged",
			order: []uint64{1, 2, 3},
			want:  []bool{true, true, true},
		},
		{
			name:  "older than the window is treated as seen",
			order: []uint64{seenWindow + 10, 1},
			want:  []bool{true, false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSeenSeqs()
			for i, seq := range tc.order {
				if got := s.admit("10.0.0.1:9102", seq); got != tc.want[i] {
					t.Errorf("admit(%d) = %v, want %v", seq, got, tc.want[i])
				}
			}
		})
	}
}

// Two origins are independent: one member's numbering says nothing about
// another's.
func TestSeenSeqsIsPerOrigin(t *testing.T) {
	s := newSeenSeqs()
	if !s.admit("a:1", 7) || !s.admit("b:1", 7) {
		t.Fatal("the same sequence from two origins is not the same event")
	}
	if s.admit("a:1", 7) {
		t.Error("a repeat from the same origin was admitted")
	}
}

// Restoring the redundancy removes the accidental ordering the high-water mark
// provided, so handlers that would overwrite newer state with older refuse. The
// rows are the four the audit found; each is a state that would otherwise
// survive for ever, or until a timeout.
func TestOrderGuardRefusesStaleEventsPerObject(t *testing.T) {
	tests := []struct {
		name string
		key  string
		what string
	}{
		{"a session replica", "sess:abc", "a late OPEN after CLOSE resurrects a replica nothing removes again"},
		{"a kill", "kill:42", "a late KILLING after DONE recreates a hold only the TTL ends"},
		{"a kick", "kick:u@d.test", "a late kick kills the user's NEW session"},
		{"a member", "member:10.0.0.5:9102", "a late ADD after REMOVE brings a departed member back"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newOrderGuard()
			if !g.admit(tc.key, 10) {
				t.Fatal("the first event was refused")
			}
			if g.admit(tc.key, 9) {
				t.Errorf("an older event was applied: %s", tc.what)
			}
			if !g.admit(tc.key, 11) {
				t.Error("a newer event was refused")
			}
		})
	}
}

// The blocker the review caught: sequence numbers are only meaningful within
// the member that issued them -- in the sandbox the three directors sat at 114,
// 138 and 8 -- so comparing across origins would refuse a fresh event from a
// member whose counter is lower. An admin move lands on whichever director the
// client reached, so the second move of one user is routinely issued by a
// different member than the first: guarding by object alone would have turned
// the selective loss this change removes into a deterministic one.
func TestOrderGuardDoesNotCompareAcrossOrigins(t *testing.T) {
	g := newOrderGuard()
	const object = "kick:u@d.test"

	if !g.admit(object+"@10.0.0.1:9102", 138) {
		t.Fatal("the first event was refused")
	}
	// Another member, lower counter, genuinely later event.
	if !g.admit(object+"@10.0.0.2:9102", 8) {
		t.Error("a fresh event from another member was refused as stale -- " +
			"its counter is unrelated to the first member's")
	}
	// The same member repeating itself is still refused.
	if g.admit(object+"@10.0.0.1:9102", 137) {
		t.Error("a stale repeat from the same member was applied")
	}
}

// The same property through the envelope path, which is where the keying
// actually happens: the unit row above exercises the guard directly and cannot
// see how the dispatcher builds its key. This one fails if the origin is left
// out of it -- the blocker as it would reach the field.
func TestEnvelopeGuardKeysIncludeTheOrigin(t *testing.T) {
	s := NewWithOptions(Options{UserKillConfirmGrace: time.Millisecond, UserKillTimeout: time.Minute})
	const user = "u9@d.test"
	hash := HashUsername(user, s.hf)

	deliver := func(ip string, seq uint64) {
		s.membership.handleEnvelope([]string{
			"USER-KILLING", ip, "9102", fmt.Sprintf("%d", seq),
			fmt.Sprintf("%d", hash), "60000",
		}, nil)
	}

	// A member far along its own counter, then a different member early in its
	// own -- an admin move landing on another director, which is routine.
	deliver("10.0.0.1", 138)
	s.applyKillDone(hash)
	deliver("10.0.0.2", 8)

	if !s.isKilling(hash) {
		t.Fatal("a kill from a second member was refused because its counter is lower than " +
			"the first member's -- counters are per member and mean nothing across them")
	}
}

// The field case, end to end through the envelope path: a batch of MOVED,
// KILLING and KICKED from one originator, where the direct copy of KILLING is
// lost and its relayed copy arrives after MOVED. The kill must still be
// recorded -- which is the whole failure the sandbox showed, where the move
// landed, the kill never did, and the originator timed out (#1359).
func TestALateKillingEnvelopeStillArmsTheHold(t *testing.T) {
	s := NewWithOptions(Options{UserKillConfirmGrace: time.Millisecond, UserKillTimeout: time.Minute})
	const user = "u1@d.test"
	hash := HashUsername(user, s.hf)
	origin := []string{"10.9.9.9", "9102"}

	deliver := func(kind string, seq uint64, payload ...string) {
		fields := append([]string{kind, origin[0], origin[1], fmt.Sprintf("%d", seq)}, payload...)
		s.membership.handleEnvelope(fields, nil)
	}

	// The batch as the originator sent it: MOVED (5), KILLING (6), KICKED (7).
	// The direct copy of 6 is lost, so 7 and then 5 arrive first by other paths.
	deliver("USER-MOVED", 5, user, "10.0.0.2:993")
	deliver("USER-KICKED", 7, user, "10.0.0.1")
	// ... and the relayed copy of the kill finally lands, out of order.
	deliver("USER-KILLING", 6, fmt.Sprintf("%d", hash), "60000")

	if !s.isKilling(hash) {
		t.Fatal("the late KILLING was dropped as stale, so this member never held LOOKUP " +
			"-- the shape the sandbox saw, where the move landed and the kill did not")
	}
}
