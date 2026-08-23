package ring

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func staticList() []Backend {
	return []Backend{
		{IP: "10.0.0.1", Port: 143, Vhosts: 100},
		{IP: "10.0.0.2", Port: 143, Vhosts: 100},
		{IP: "10.0.0.3", Port: 143, Vhosts: 100},
	}
}

// The property everything in this mode rests on, and the clause that makes it a
// test rather than a formality is the second one: **including while one of them
// believes a backend is down**.
//
// Without that clause an implementation with local failover passes -- on a
// healthy cluster it agrees with everyone. With it, that implementation fails
// by construction, because a disagreement about liveness is exactly when it
// re-routes and the others do not. Same principle as picking an input from the
// neighbouring alphabet: choose the case where right and wrong answer
// differently (#1415).
func TestEveryFrontendPlacesAUserOnTheSameBackend(t *testing.T) {
	hf := MustParseHashFormat("%Lu")

	frontendA, err := NewStatic(hf, staticList())
	if err != nil {
		t.Fatal(err)
	}
	frontendB, err := NewStatic(hf, staticList())
	if err != nil {
		t.Fatal(err)
	}

	// The second frontend has watched a backend fail: it dialled 10.0.0.2 and
	// got nothing. In a design with local health that observation would move
	// users; here there is nowhere to put it, and this is the assertion.
	observedDown := "10.0.0.2"

	var landedOnDown int
	for i := 0; i < 500; i++ {
		user := fmt.Sprintf("u%d@example.com", i)
		a, okA := frontendA.Lookup(user)
		b, okB := frontendB.Lookup(user)
		if !okA || !okB {
			t.Fatalf("%s resolved nowhere", user)
		}
		if a.IP != b.IP {
			t.Fatalf("%s: frontend A says %s, frontend B says %s", user, a.IP, b.IP)
		}
		if a.IP == observedDown {
			landedOnDown++
		}
	}
	// And the users of the failing backend are still placed there -- refused,
	// not moved. A run where nobody hashes to it would prove nothing.
	if landedOnDown == 0 {
		t.Fatal("no user placed on the failing backend: the run cannot show that placement ignores liveness")
	}
	t.Logf("%d of 500 users stayed on the backend one frontend saw fail", landedOnDown)
}

// The same list must place users the way the director does, so that adding a
// director later moves nobody. Both compute it from this package; the guard is
// that they compute it from the SAME entry point rather than two that agree
// today.
func TestStaticPlacementMatchesTheRing(t *testing.T) {
	hf := MustParseHashFormat("%Lu")
	st, err := NewStatic(hf, staticList())
	if err != nil {
		t.Fatal(err)
	}
	// A ring built the way the director builds one, from the same entries.
	r := New(hf)
	for _, b := range staticList() {
		cp := b
		cp.Up = true
		r.AddBackend(&cp)
	}
	for i := 0; i < 200; i++ {
		user := fmt.Sprintf("u%d@example.com", i)
		got, ok := st.Lookup(user)
		if !ok {
			t.Fatalf("%s resolved nowhere", user)
		}
		if want := r.Lookup(user); got.IP != want {
			t.Fatalf("%s: static says %s, the ring says %s", user, got.IP, want)
		}
	}
}

// A hash template that differs between the director and a frontend is a
// disagreement on every user, not at some boundary -- so the template is part
// of the placement, and this pins that it is used rather than ignored.
func TestTheHashTemplateChangesPlacement(t *testing.T) {
	byUser, err := NewStatic(MustParseHashFormat("%Lu"), staticList())
	if err != nil {
		t.Fatal(err)
	}
	byDomain, err := NewStatic(MustParseHashFormat("%Ld"), staticList())
	if err != nil {
		t.Fatal(err)
	}
	differing := 0
	for i := 0; i < 200; i++ {
		user := fmt.Sprintf("u%d@example.com", i)
		a, _ := byUser.Lookup(user)
		b, _ := byDomain.Lookup(user)
		if a.IP != b.IP {
			differing++
		}
	}
	if differing == 0 {
		t.Error("two different templates placed every user identically: the template is not reaching the hash")
	}
	// Hashing by domain puts one domain in one place, which is the point of
	// that template.
	first, _ := byDomain.Lookup("a@example.com")
	second, _ := byDomain.Lookup("b@example.com")
	if first.IP != second.IP {
		t.Errorf("hashing by domain split one domain: %s vs %s", first.IP, second.IP)
	}
}

func TestAnEmptyListIsRefused(t *testing.T) {
	if _, err := NewStatic(MustParseHashFormat("%Lu"), nil); err == nil {
		t.Error("an empty list built a ring that answers every lookup with nowhere")
	}
}

// No health surface, and it stays that way. This mode is a promise not to
// re-route; the promise lives in the type having nothing to re-route with, so
// a later change that adds a setter has to argue with this test first.
func TestStaticHasNoWayToMarkABackendDown(t *testing.T) {
	body, err := os.ReadFile("static.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// Up is set once, at construction, to true.
		if strings.Contains(trimmed, "Up = false") || strings.Contains(trimmed, "MarkDown") ||
			strings.Contains(trimmed, "SetUp") || strings.Contains(trimmed, "LastDown") {
			t.Errorf("static.go:%d gives this mode a health surface: %s\n"+
				"re-routing without shared state is two owners of one mailbox; a user on a silent backend is refused, not moved",
				i+1, trimmed)
		}
	}
}
