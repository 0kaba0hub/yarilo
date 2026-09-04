package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// Reached only by a caller that built its own identity: dropping the seed in
// Resolver.UserInfo or LockID puts this back on live paths (#1670).
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

// ownerForm is what every acquisition must announce: proc/pid/user/id, the
// string Owner builds.
const ownerForm = "proc/pid/user/id"

// CheckOwner guards the boundary with the lock service, returning the owner to
// announce. It stands here, not in Owner, because a caller that builds its own
// string never calls Owner (#1672).
func CheckOwner(owner string) string {
	if ownerWellFormed(owner) {
		return owner
	}
	_, file, line, _ := runtime.Caller(2)
	if underTest() {
		panic(fmt.Sprintf("locks: owner %q is not %s at %s:%d -- build it with locks.Owner (#1672)",
			owner, ownerForm, file, line))
	}
	repaired := repairOwner(owner)
	slog.Error("locks: owner is not in the "+ownerForm+" form; announcing a repaired one",
		"owner", owner, "caller", fmt.Sprintf("%s:%d", file, line), "announced", repaired)
	return repaired
}

// repairOwner puts a foreign string in the user segment rather than dropping it,
// so held_by still shows what the caller called itself. Its separators become
// colons: a reader splitting on "/" must find four fields whatever it carried.
func repairOwner(owner string) string {
	user := strings.ReplaceAll(owner, "/", ":")
	if user == "" {
		user = "unknown"
	}
	return fmt.Sprintf("%s/%d/%s/%s", procName(), os.Getpid(), user, anonID())
}

// ownerWellFormed reports whether owner is proc/pid/user/id with all four parts
// present and the pid numeric.
func ownerWellFormed(owner string) bool {
	parts := strings.Split(owner, "/")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	_, err := strconv.Atoi(parts[1])
	return err == nil
}

// ctxIDKey carries the id of the work in flight -- a session, a delivery, a
// request -- to a lock deep enough that passing it as an argument would mean
// threading it through interfaces that have nothing to do with locking.
type ctxIDKey struct{}

// WithID returns a context carrying id as the lock identity of the work in it.
func WithID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxIDKey{}, id)
}

// IDFrom returns the id WithID put in ctx, or "" -- which Owner then reports as
// the defect it is, at the entry point that failed to set one.
func IDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxIDKey{}).(string)
	return id
}

// SiteUnknown is what a holder announces when nothing said what it was doing:
// an older client, or a call site that has not been given a site yet.
const SiteUnknown = "unknown"

// ctxSiteKey carries what the work in flight is doing to the acquisition, the
// way ctxIDKey carries who it is.
type ctxSiteKey struct{}

// WithSite returns a context whose acquisitions announce site.
func WithSite(ctx context.Context, site string) context.Context {
	if site == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxSiteKey{}, site)
}

// SiteFrom returns the site WithSite put in ctx, or "" when it carries none.
func SiteFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxSiteKey{}).(string)
	return s
}

// CheckSite is CheckOwner's twin: a context with no site is a defect at the call
// site. The server's own unknown is different and stays (#1676).
func CheckSite(ctx context.Context) string {
	if s := SiteFrom(ctx); s != "" {
		return s
	}
	_, file, line, _ := runtime.Caller(2)
	if underTest() {
		panic(fmt.Sprintf("locks: no site in the context at %s:%d -- wrap it with "+
			"locks.WithSite (#1676)", file, line))
	}
	slog.Error("locks: acquisition with no site; announcing unknown",
		"caller", fmt.Sprintf("%s:%d", file, line))
	return SiteUnknown
}
