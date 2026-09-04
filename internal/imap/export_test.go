package imap

import (
	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SetTestSessionID stands in for the login proxy's preamble, so a test
// connection can carry a session id and the two owner spellings stay
// distinguishable (#1652).
func SetTestSessionID(id string) { testSessionID = id }

// SetEnvCacheObserver watches the options every FETCH opens the envelope cache
// with, so the sharing decision can be asserted without racing two clients.
func SetEnvCacheObserver(fn func(msgcache.Options)) func() {
	prev := openEnvCache
	openEnvCache = func(idx mailbox.UserIndex, folderID uint64, o msgcache.Options) *msgcache.Handle {
		fn(o)
		return prev(idx, folderID, o)
	}
	return func() { openEnvCache = prev }
}
