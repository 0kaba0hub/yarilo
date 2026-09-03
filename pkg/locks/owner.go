package locks

import (
	"fmt"
	"os"
	"path/filepath"
)

// Owner is the one spelling of "<process>/<pid>/<user>[/<session>]". There were
// six, and held_by said three different things about one holder (#1647). Every
// lock in the tree is one user's, so the user is always known.
//
// The session is part of the identity on purpose -- the string answers "which
// session holds this" -- so distinct owners count sessions, not holders. A
// metric that labels by owner must say that in its help text, because a reader
// of a metric does not read this comment (#1645).
func Owner(user, sessionID string) string {
	proc := "yarilo"
	if len(os.Args) > 0 {
		proc = filepath.Base(os.Args[0])
	}
	if sessionID == "" {
		return fmt.Sprintf("%s/%d/%s", proc, os.Getpid(), user)
	}
	return fmt.Sprintf("%s/%d/%s/%s", proc, os.Getpid(), user, sessionID)
}
