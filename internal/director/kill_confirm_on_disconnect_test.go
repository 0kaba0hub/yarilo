package director

import (
	"fmt"
	"testing"
	"time"
)

// A login pod's connection dying mid-kill takes that user's sessions with it,
// and that IS the count reaching zero. The confirm has to be armed by it, the
// same as by an ordinary SESSION-CLOSE or by the reconciliation -- otherwise
// the kill waits out its timeout while the thing it waits for has already
// happened (#1393, the shape of #1359).
func TestConnectionDeathArmsTheKillConfirm(t *testing.T) {
	srv, addr := startServer(t)
	const user = "u@example.com"
	hash := HashUsername(user, srv.hf)

	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)
	fmt.Fprintf(conn, "SESSION-OPEN\ts1\t%s\t10.0.0.20\timap\n", user)
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("SESSION-OPEN: %q", got)
	}

	// The user is being killed, and the session is what the confirm waits for.
	srv.startKilling(hash)

	conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		srv.killMu.Lock()
		defer srv.killMu.Unlock()
		st, ok := srv.killing[hash]
		return ok && !st.zeroSince.IsZero()
	})
}

// The arming must follow the count actually reaching zero. The killed user
// here has sessions on TWO login connections; one dying leaves the other, so
// the confirm must stay unarmed -- a connection death is evidence about that
// connection, not about the user.
func TestPartialDisconnectDoesNotArmTheConfirm(t *testing.T) {
	srv, addr := startServer(t)
	const killed = "u@example.com"
	hash := HashUsername(killed, srv.hf)

	first, firstSc := dialTest(t, addr)
	readHandshake(t, firstSc)
	sendHandshake(t, first)
	fmt.Fprintf(first, "SESSION-OPEN\ts1\t%s\t10.0.0.20\timap\n", killed)
	readLine(t, firstSc)

	second, secondSc := dialTest(t, addr)
	readHandshake(t, secondSc)
	sendHandshake(t, second)
	fmt.Fprintf(second, "SESSION-OPEN\ts2\t%s\t10.0.0.21\timap\n", killed)
	readLine(t, secondSc)

	srv.startKilling(hash)
	first.Close() // one of the two goes; the user is still connected

	time.Sleep(300 * time.Millisecond)
	srv.killMu.Lock()
	st, ok := srv.killing[hash]
	armed := ok && !st.zeroSince.IsZero()
	srv.killMu.Unlock()
	if armed {
		t.Error("the confirm was armed while the killed user still has a session open")
	}
}
