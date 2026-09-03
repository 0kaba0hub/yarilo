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
// An empty id is a defect at the entry point that built the string, not a state
// this function serves: three rounds of chasing anonymous holders were spent
// repairing places that lost the id instead of making the id mandatory (#1670).
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

// NewID mints a session, delivery or request identifier: a hex-encoded random
// 8-byte handle. Every entry point that owns a lock mints one when nothing
// upstream handed it one.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// anonID is what a caller that lost its id announces instead: minted once, so
// the process is still one identity in held_by rather than a new stranger per
// acquisition, and prefixed so a reader knows the id is the fallback and not
// something a session actually carried.
var anonID = sync.OnceValue(func() string { return "noid-" + NewID() })

// underTest reports whether this is a `go test` binary, where an empty id fails
// loudly rather than being logged and carried on with. Looked up on the call,
// not at init: testing registers its flags after package variables run, so a
// var here would read nil and the check would never fire under test.
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
