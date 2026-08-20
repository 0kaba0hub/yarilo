package director

import (
	"strings"
	"testing"
)

// TestMoveKick_LoginLineCarriesTheUsernameOnly is the #1363 seam: the director
// that AUTHORS a move writes to its own login clients directly, and a login
// attached to it saw the ring's old-backend field glued onto the username.
// Asserted on the wire the login actually reads, because both the builder and
// the write path were part of the defect.
//
// The ring form is unchanged and still carries the old backend — the two are
// different protocols and this test pins the difference.
func TestMoveKick_LoginLineCarriesTheUsernameOnly(t *testing.T) {
	srv, addr := startServer(t)

	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)

	// Pin the user somewhere, then move: a genuine relocation kicks the old
	// sessions, which is the only path that builds a conditional kick.
	srv.userDir.Set("u@example.com", "10.0.0.20:993", false)
	srv.moveUser("u@example.com", "10.0.0.30:993", nil)

	var kick string
	for i := 0; i < 8 && kick == ""; i++ {
		line := readLine(t, loginSc)
		if strings.HasPrefix(line, "USER-KICKED\t") {
			kick = line
		}
	}
	if kick == "" {
		t.Fatal("the moving director never pushed USER-KICKED to its own login client")
	}
	if want := "USER-KICKED\tu@example.com"; kick != want {
		t.Errorf("login kick line = %q, want %q — the ring's old-backend field must not reach a login", kick, want)
	}
}

// TestLoginKickLine_IsTheOnlyBuilder: the originator and a peer relaying the
// same kick must produce identical bytes. They diverged once; a single builder
// is what stops them diverging again.
func TestLoginKickLine_IsTheOnlyBuilder(t *testing.T) {
	srv, addr := startServer(t)
	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)

	// Arrived from a peer, conditional form (user + old backend).
	srv.membership.applyEnvelope("USER-KICKED", []string{"u@example.com", "10.0.0.20"}, "10.0.0.99:9102", 7)
	relayed := readLine(t, loginSc)

	if want := loginKickLine("u@example.com"); relayed != want {
		t.Fatalf("relayed kick = %q, want %q", relayed, want)
	}
}
