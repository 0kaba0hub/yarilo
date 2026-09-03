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

	if n := len(strings.Split(withSession, "/")); n != 4 {
		t.Errorf("owner %q has %d segments, want 4", withSession, n)
	}
}

// An empty id is a defect at the caller, not a spelling this function serves.
//
// It used to be legal and produced a three-segment name. Three rounds of
// diagnosis went into repairing the places that lost the id; what none of them
// could fix is that losing it was allowed (#1670).
func TestAnEmptyIDIsRejected(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Owner accepted an empty session id: an anonymous holder reaches the " +
				"lock service and held_by cannot say who it is")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "empty session id") {
			t.Errorf("panic did not name the defect: %v", r)
		}
	}()
	_ = Owner("u1@example.com", "")
}
