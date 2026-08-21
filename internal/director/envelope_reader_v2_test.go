package director

import (
	"fmt"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// #1365 step 1: the reader accepts both forms of a user event. The form is
// announced by the kind, because it cannot be told from the bytes -- and the
// name below is why. A backslash is legal in a quoted-string localpart, and a
// blind TabUnescape would turn this name into one carrying a real TAB.
const backslashUser = `a\tb@example.com`

// Driven through handleRingLine -- the dispatcher -- because what decides
// whether a kind is known, forwarded and normalised lives there; a test of the
// normaliser alone would keep passing if the kind fell out of the switch.
func TestReaderAcceptsBothFormsOfUserEvents(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		user     string // as it goes on the wire
		wantUser string // as the directory must see it
	}{
		{
			name:     "plain kind carries the name raw",
			kind:     "USER-KICKED",
			user:     "plain@example.com",
			wantUser: "plain@example.com",
		},
		{
			name:     "escaped kind is decoded",
			kind:     "USER-KICKED-ESC",
			user:     proto.TabEscape(backslashUser),
			wantUser: backslashUser,
		},
		{
			// The distinguishing row: a name whose bytes look escaped but are
			// not. Read as the plain kind it must survive untouched -- a
			// reader that unescaped by inspection would corrupt it here.
			name:     "a raw name that looks escaped is left alone",
			kind:     "USER-KICKED",
			user:     backslashUser,
			wantUser: backslashUser,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startServer(t)
			m := srv.membership
			hash := HashUsername(tc.wantUser, srv.hf)
			srv.userDir.Set(tc.wantUser, "10.0.0.20:993", false)

			m.handleRingLine([]string{tc.kind, "10.0.0.99", "9102", "1", tc.user}, nil)

			// A kick drops the sticky pin for exactly that user. If the name
			// arrived mangled, some other user's pin (or none) was cleared.
			if e := srv.userDir.GetByHash(hash); e != nil {
				t.Errorf("the pin for %q survived the kick: %+v -- the name did not arrive intact", tc.wantUser, e)
			}
		})
	}
}

// The move kind carries the name in the same position, with fields after it:
// decoding must not disturb them.
func TestReaderDecodesMoveWithoutDisturbingTheRest(t *testing.T) {
	srv, _ := startServer(t)
	m := srv.membership
	hash := HashUsername(backslashUser, srv.hf)

	m.handleRingLine([]string{
		"USER-MOVED-ESC", "10.0.0.99", "9102", "1",
		proto.TabEscape(backslashUser), "10.0.0.30", "993",
	}, nil)

	e := srv.userDir.GetByHash(hash)
	if e == nil {
		t.Fatal("the move did not reach the directory: the decoded name did not match")
	}
	if e.Host != "10.0.0.30:993" {
		t.Errorf("backend = %q, want 10.0.0.30:993 -- decoding shifted the fields after the name", e.Host)
	}
}

// #1362: an origin field may carry an incarnation. Two instances on the same
// address are the same member to the ring and different origins to dedup, so
// the second one's events are not mistaken for ones already applied.
func TestIncarnationMakesARestartANewOrigin(t *testing.T) {
	srv, _ := startServer(t)
	m := srv.membership
	user := "moved@example.com"
	hash := HashUsername(user, srv.hf)

	// The predecessor's events climb its counter.
	for seq := 1; seq <= 5; seq++ {
		m.handleRingLine([]string{
			"USER-MOVED", "10.0.0.99@first", "9102", fmt.Sprintf("%d", seq),
			user, "10.0.0.30", "993",
		}, nil)
	}
	if e := srv.userDir.GetByHash(hash); e == nil || e.Host != "10.0.0.30:993" {
		t.Fatalf("setup: predecessor's move did not apply: %+v", e)
	}

	// It restarts on the same address: same member, new incarnation, counter
	// back to 1. Without the incarnation this is seq 1 from an origin already
	// seen at 5, and the ordering guard refuses it.
	m.handleRingLine([]string{
		"USER-MOVED", "10.0.0.99@second", "9102", "1",
		user, "10.0.0.40", "993",
	}, nil)

	e := srv.userDir.GetByHash(hash)
	if e == nil || e.Host != "10.0.0.40:993" {
		t.Fatalf("the restarted director's first event was ignored: %+v", e)
	}
}

// The same address without an incarnation must keep behaving as before: one
// origin, and a replayed sequence number is still a duplicate.
func TestSameOriginWithoutIncarnationStillDedups(t *testing.T) {
	srv, _ := startServer(t)
	m := srv.membership
	user := "dedup@example.com"
	hash := HashUsername(user, srv.hf)

	m.handleRingLine([]string{"USER-MOVED", "10.0.0.99", "9102", "1", user, "10.0.0.30", "993"}, nil)
	m.handleRingLine([]string{"USER-MOVED", "10.0.0.99", "9102", "1", user, "10.0.0.40", "993"}, nil)

	if e := srv.userDir.GetByHash(hash); e == nil || e.Host != "10.0.0.30:993" {
		t.Errorf("a replayed (origin, seq) was applied again: %+v", e)
	}
}

// Our own event must be absorbed even when the origin field carries an
// incarnation -- which it will as soon as writers switch. An event we fail to
// absorb is one we apply to ourselves and send around the ring a second time.
func TestOwnEventIsAbsorbedDespiteIncarnation(t *testing.T) {
	srv, _ := startServerOpts(t, Options{LocalIP: "10.0.0.1", LocalPort: 9102, PingInterval: time.Hour})
	m := srv.membership
	user := "self@example.com"
	hash := HashUsername(user, srv.hf)
	srv.userDir.Set(user, "10.0.0.20:993", false)

	m.handleRingLine([]string{"USER-KICKED", "10.0.0.1@mine", "9102", "1", user}, nil)

	if e := srv.userDir.GetByHash(hash); e == nil {
		t.Error("our own kick was applied to us instead of being absorbed")
	}
}
