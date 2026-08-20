package director

import (
	"fmt"
	"testing"
)

// TestUserDir_Conflict_LoserKicksStaleSessions is the #772 PR-3 end-to-end:
// two directors pin the same user to DIFFERENT backends at the same logical
// time; the lower-id director wins, and the loser — receiving the winner's
// USER-ASSIGN — switches its own entry and kicks that user's now-stale
// sessions off the wrong backend, while leaving other users on it running.
//
// This server's self id is ":0" (default opts); an incoming assignment by
// a real "10.0.0.1:9102" id sorts lower ("1" < ":"), so at equal seq the
// incoming wins and this node is the loser.
func TestUserDir_Conflict_LoserKicksStaleSessions(t *testing.T) {
	srv, addr := startServer(t)

	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)

	// Two users' sessions on the SAME (soon-to-be-wrong) backend Y.
	loginConn.Write([]byte("SESSION-OPEN\tsU\tu@example.com\t10.0.0.20\n"))
	if got := readLine(t, loginSc); got != "OK" {
		t.Fatalf("SESSION-OPEN u: %q", got)
	}
	loginConn.Write([]byte("SESSION-OPEN\tsV\tv@example.com\t10.0.0.20\n"))
	if got := readLine(t, loginSc); got != "OK" {
		t.Fatalf("SESSION-OPEN v: %q", got)
	}

	// This director's own pin for u → Y (seq 1, by ":0").
	hu := srv.userDir.Set("u@example.com", "10.0.0.20:993", false)

	// The winner's conflicting assignment arrives: u → X, SAME seq, lower id.
	srv.membership.applyEnvelope("USER-ASSIGN",
		[]string{fmt.Sprintf("%d", hu), "10.0.0.30:993", "1", "10.0.0.1:9102"}, "10.0.0.99:9102", 1)

	// Local entry must have switched to the winner's backend.
	if e := srv.userDir.GetByHash(hu); e == nil || e.Host != "10.0.0.30:993" {
		t.Fatalf("loser must adopt the winner's backend, got %+v", e)
	}

	// u's stale session on Y must be kicked; v's must survive.
	if got := readLine(t, loginSc); got != "USER-KICKED\tu@example.com" {
		t.Fatalf("expected USER-KICKED for u, got %q", got)
	}
	srv.sessRecMu.RLock()
	_, uAlive := srv.sessById["sU"]
	_, vAlive := srv.sessById["sV"]
	srv.sessRecMu.RUnlock()
	if uAlive {
		t.Error("u's stale session must be removed")
	}
	if !vAlive {
		t.Error("v's session on the same backend must NOT be kicked")
	}
}

// TestUserDir_Conflict_WinnerKeepsAndDoesNotKick verifies the symmetric
// side: the lower-id director (the winner) receiving the loser's assignment
// keeps its own backend and kicks nothing.
func TestUserDir_Conflict_WinnerKeepsAndDoesNotKick(t *testing.T) {
	// self id "10.0.0.1:9102" — lower than the incoming "10.0.0.2:9102".
	srv, addr := startServerOpts(t, Options{LocalIP: "10.0.0.1", LocalPort: 9102})

	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)
	loginConn.Write([]byte("SESSION-OPEN\tsW\tw@example.com\t10.0.0.40\n"))
	readLine(t, loginSc) // OK

	hw := srv.userDir.Set("w@example.com", "10.0.0.40:993", false) // seq 1, by 10.0.0.1

	// Loser's assignment: w → Z, same seq, HIGHER id → we win, keep 10.0.0.40.
	srv.membership.applyEnvelope("USER-ASSIGN",
		[]string{fmt.Sprintf("%d", hw), "10.0.0.50:993", "1", "10.0.0.2:9102"}, "10.0.0.99:9102", 2)

	if e := srv.userDir.GetByHash(hw); e == nil || e.Host != "10.0.0.40:993" {
		t.Fatalf("winner must keep its backend, got %+v", e)
	}
	srv.sessRecMu.RLock()
	_, alive := srv.sessById["sW"]
	srv.sessRecMu.RUnlock()
	if !alive {
		t.Error("winner must not kick its own session")
	}
}
