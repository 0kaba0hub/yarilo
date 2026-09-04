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

// The boundary refuses an owner that did not come from Owner.
//
// The guard used to stand in Owner, which only ever saw callers who went
// through it. sieve built "sieve:<pid>" by hand and reached the lock service
// under it for as long as the code existed, past a sentinel and past two audits
// that each declared themselves complete (#1672). Standing on the boundary, the
// check sees every acquisition whatever built the string.
func TestTheBoundaryRefusesAForeignOwnerSpelling(t *testing.T) {
	for _, owner := range []string{
		"sieve:7",                  // the spelling the field showed
		"backendapi/folder.create", // an operation name, no user, no id
		"proc/pid/user/id",         // four parts, but the pid is not a number
		"yarilo-imap/7/u@x.com",    // the three-segment form
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("owner %q reached the lock service unchallenged", owner)
				}
			}()
			_ = CheckOwner(owner)
		}()
	}
	if got := CheckOwner(Owner("u@x.com", "sess1")); got == "" {
		t.Error("a well-formed owner was not returned unchanged")
	}
}

// The repaired form is four fields whatever the caller carried.
//
// Asserted on repairOwner directly: CheckOwner panics under test, so the branch
// that builds this string is unreachable from there and nothing else states what
// it produces. A foreign string with slashes in it -- which is most of the ones
// the field showed -- would otherwise make held_by five or six fields, and every
// reader splitting on "/" would read the wrong one.
func TestARepairedOwnerStaysFourFields(t *testing.T) {
	for _, owner := range []string{
		"sieve:7",
		"backendapi/folder.create",
		"yarilo-imap/7/u@x.com",
		"",
	} {
		got := repairOwner(owner)
		if n := len(strings.Split(got, "/")); n != 4 {
			t.Errorf("repairOwner(%q) = %q, %d fields, want 4", owner, got, n)
		}
		if !ownerWellFormed(got) {
			t.Errorf("repairOwner(%q) = %q, which the guard would reject in turn", owner, got)
		}
	}
	if got := repairOwner("yarilo-imap/7/u@x.com"); !strings.Contains(got, "yarilo-imap:7:u@x.com") {
		t.Errorf("repairOwner dropped what the caller called itself: %q", got)
	}
}
