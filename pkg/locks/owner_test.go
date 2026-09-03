package locks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session is carried, and it is the last segment. The end-to-end test in
// the imap package cannot reach this: a session id arrives from the login
// proxy's preamble, so an ordinary test connection has none and every path
// there produces the sessionless form. Tested here rather than left to a run.
func TestOwnerCarriesTheSession(t *testing.T) {
	proc := filepath.Base(os.Args[0])
	pid := os.Getpid()

	withSession := Owner("u1@example.com", "4hZsLQ3Cbjz")
	want := fmt.Sprintf("%s/%d/%s/%s", proc, pid, "u1@example.com", "4hZsLQ3Cbjz")
	if withSession != want {
		t.Errorf("owner = %q, want %q", withSession, want)
	}

	// Two sessions of one user are two owners. That is the point of the field
	// and the reason a count of distinct owners counts sessions (#1645).
	other := Owner("u1@example.com", "4hZsLQ5B95z")
	if other == withSession {
		t.Error("two sessions of one user produced one owner: held_by cannot say which holds it")
	}

	// And without one, the user is still there. A caller with no session must
	// not fall back to naming only itself, which is what yarilo-fts did while
	// holding a per-user key.
	sessionless := Owner("u1@example.com", "")
	if !strings.HasSuffix(sessionless, "/u1@example.com") {
		t.Errorf("sessionless owner = %q, which does not end in the user it holds for", sessionless)
	}
	if strings.HasPrefix(sessionless, withSession) || sessionless == withSession {
		t.Errorf("the sessionless form is not distinct from the session one: %q against %q",
			sessionless, withSession)
	}
}
