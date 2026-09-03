package locks

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Owner is the one spelling of "<process>/<pid>/<user>/<session>". There were
// six, and held_by said three different things about one holder (#1647). Every
// lock in the tree is one user's, so the user is always known.
//
// The session is part of the identity on purpose -- the string answers "which
// session holds this" -- so distinct owners count sessions, not holders. A
// metric that labels by owner must say that in its help text, because a reader
// of a metric does not read this comment (#1645).
//
// An empty id is a defect at the entry point, not a state this serves: three
// rounds went into repairing places that lost it, not into requiring it (#1670).
func Owner(user, sessionID string) string {
	if sessionID == "" {
		sessionID = reportMissingID(user)
	}
	return fmt.Sprintf("%s/%d/%s/%s", procName(), os.Getpid(), user, sessionID)
}

func procName() string {
	if len(os.Args) > 0 {
		return filepath.Base(os.Args[0])
	}
	return "yarilo"
}

// NewID mints a session, delivery or request identifier: hex-encoded random 8
// bytes. Every entry point that owns a lock mints one when given none.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// anonID is what a caller that lost its id announces: minted once, and prefixed
// so a reader knows no session actually carried it.
var anonID = sync.OnceValue(func() string { return "noid-" + NewID() })

// underTest reports whether this is a `go test` binary, where an empty id
// panics. Looked up on the call, not at init: testing registers its flags after
// package variables run, so a var here reads nil and never fires.
func underTest() bool { return flag.Lookup("test.v") != nil }

func reportMissingID(user string) string {
	_, file, line, _ := runtime.Caller(2)
	if underTest() {
		panic(fmt.Sprintf("locks.Owner: empty session id for %q at %s:%d -- "+
			"the entry point must mint one (#1670)", user, file, line))
	}
	slog.Error("locks: lock owner built without a session id; announcing the process fallback",
		"user", user, "caller", fmt.Sprintf("%s:%d", file, line), "id", anonID())
	return anonID()
}
