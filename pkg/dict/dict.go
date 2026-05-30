// Package dict is yarilo's pluggable key-value store abstraction.
//
// It provides a single contract — Dict + Tx + Iterator — that every
// feature needing durable per-user or per-mailbox state speaks:
// METADATA (RFC 5464), quota counters, ACL state, sieve script
// indices, future replication cursors. Concrete storage choices
// (a local JSON file for standalone, Redis for shared cluster
// state, PostgreSQL for operators who already run one) live behind
// drivers and are selected via config, not code.
//
// Driver authors implement the Dict + Tx + Iterator interfaces and
// register themselves via Register("name", initFunc). Callers obtain
// a Dict via Open(Config{...}); the driver name in Config.Driver
// selects which factory runs.
package dict

import (
	"context"
	"strings"
)

// Path prefixes reserved at the application level so that per-user
// (priv/) and per-shared-resource (shared/) namespaces can co-exist
// in a single dict instance. Drivers do not interpret these prefixes;
// the convention is enforced by callers.
const (
	PathPrivate = "priv/"
	PathShared  = "shared/"
)

// IterFlag is a bitmask of options for Dict.Iterate.
type IterFlag uint32

const (
	// IterRecurse descends into sub-hierarchies. Without it, iterating
	// "foo/" returns "foo/a" but not "foo/a/b".
	IterRecurse IterFlag = 1 << iota
	// IterSortByKey returns rows in ascending key order.
	IterSortByKey
	// IterSortByValue returns rows in ascending value order.
	// Drivers that cannot sort by value MAY fall back to insertion order.
	IterSortByValue
	// IterNoValue skips loading values — useful for cheap key listings.
	IterNoValue
	// IterExactKey returns every value bound to the exact key (multi-value
	// dicts). Equivalent to Lookup but iterator-shaped.
	IterExactKey
)

// OpSettings is the per-call context passed to every dict operation.
// It carries the username and home dir for path expansion (file driver),
// per-call TTL (drivers that support it), and a few hints used by remote
// drivers (no-slowness-warning, hide-log-values).
type OpSettings struct {
	// Username of the operation's owner. Required by drivers that
	// scope keys per-user (e.g. file path "%h/.metadata" relies on it).
	Username string
	// HomeDir of the user, if known. Used for "%h" expansion in the
	// file driver's path; left empty when the caller does not have it.
	HomeDir string
	// ExpireSecs, when non-zero, asks the driver to add a TTL of this
	// many seconds to every key written by the transaction or set call.
	// Drivers without TTL support silently ignore it.
	ExpireSecs uint32
	// NoSlownessWarning suppresses the driver's slow-commit warning log.
	// Set for transactions whose latency does not matter (e.g. best-
	// effort metadata writes).
	NoSlownessWarning bool
	// HideLogValues redacts values from any debug/trace logs the driver
	// might emit. Set when values contain sensitive data.
	HideLogValues bool
}

// CommitResult is the outcome of Tx.Commit. A driver returns CommitOK
// on success, CommitNotFound when atomic-inc targeted a missing key,
// CommitFailed on a hard error, and CommitWriteUncertain when a
// remote write could not be confirmed (e.g. network timeout). Callers
// MAY treat CommitWriteUncertain as success-with-warning depending on
// their durability requirements.
type CommitResult int

const (
	CommitOK             CommitResult = 1
	CommitNotFound       CommitResult = 0
	CommitFailed         CommitResult = -1
	CommitWriteUncertain CommitResult = -2
)

// Dict is the read/write contract every driver implements.
//
// All read methods (Lookup, Iterate) honour context cancellation;
// drivers that block on I/O MUST return ctx.Err() promptly. Mutations
// flow through Begin → Tx; there is no direct Set/Unset on Dict by
// design — the transaction-only mutation surface keeps drivers'
// write paths consistent. Close releases all driver resources;
// subsequent calls on a closed Dict return ErrClosed.
type Dict interface {
	// Lookup returns every value bound to key, plus found=true when at
	// least one value exists. The values slice is nil when found=false.
	// Drivers that store at most one value per key return a 1-element
	// slice. Honour ctx for cancellation.
	Lookup(ctx context.Context, set *OpSettings, key string) (values [][]byte, found bool, err error)

	// Iterate returns an Iterator over keys under path. The flags
	// bitmask controls recursion, sorting, value-skipping and exact-key
	// mode. The iterator MUST be Closed even when iteration stops early.
	Iterate(ctx context.Context, set *OpSettings, path string, flags IterFlag) (Iterator, error)

	// Begin opens a new transaction context. Mutations are buffered in
	// the Tx and applied atomically on Commit. Drivers that lack native
	// atomic multi-key writes simulate them via locking or write-ahead.
	Begin(ctx context.Context, set *OpSettings) (Tx, error)

	// ExpireScan asks the driver to drop expired entries. Drivers
	// without TTL support return nil (a no-op). Long-running drivers
	// (SQL) MAY do this in chunks bounded by ctx.
	ExpireScan(ctx context.Context) error

	// Wait blocks until every async operation kicked off by this Dict
	// has finished. Sync drivers (file, memory, fail) return nil
	// immediately. Reserved for the future redis async-pipeline path.
	Wait(ctx context.Context) error

	// Close releases all resources (open files, network connections,
	// background goroutines). Idempotent.
	Close() error

	// Name returns the driver name this Dict was opened with. Useful
	// for diagnostics and logging.
	Name() string
}

// Iterator yields key/value pairs from Dict.Iterate. Drivers MUST
// support repeated Next() until it returns false; after that, Err()
// reports any I/O error. Close() releases iterator resources and is
// safe to call multiple times.
type Iterator interface {
	// Next advances to the next row. Returns false when iteration ends
	// (Err may carry an error in that case) or when ctx was cancelled.
	Next() bool
	// Key returns the current row's key. Valid only after a true Next().
	Key() string
	// Values returns every value for the current key. Single-value
	// drivers return a 1-element slice. Nil when IterNoValue was set.
	Values() [][]byte
	// Err returns the first iteration error, or nil if iteration ended
	// cleanly. Always check after the loop.
	Err() error
	// Close releases resources. Idempotent. Safe to call before Next()
	// returns false (early-exit iteration).
	Close() error
}

// Tx buffers mutations until Commit. Drivers MAY apply writes
// optimistically (e.g. SQL transactions) or defer them entirely until
// commit time (e.g. memory driver). Rollback discards every pending
// mutation. Set/Unset/AtomicInc are NEVER visible to other Dict
// callers before Commit.
type Tx interface {
	// Set assigns value to key. Overwrites any prior value.
	// For multi-value drivers (SQL), appends to the value list when
	// the driver-specific "append" flag is enabled; otherwise replaces.
	Set(key string, value []byte) error
	// Unset removes key (and all its values for multi-value drivers).
	// Unsetting a missing key is not an error.
	Unset(key string) error
	// AtomicInc atomically adds delta (positive or negative) to the
	// integer value of key. If key is missing at commit time, Commit
	// returns CommitNotFound. Values are stored as decimal strings.
	AtomicInc(key string, delta int64) error
	// Commit flushes the buffered mutations. Returns CommitOK on
	// success; see CommitResult docs for the other values.
	Commit() (CommitResult, error)
	// Rollback discards every buffered mutation. Idempotent.
	Rollback() error
}

// Escape converts a string into a value safe to embed in a dict key
// path component. It percent-encodes '/' and '%' so that downstream
// path-splitting on '/' is unambiguous.
func Escape(s string) string {
	if !strings.ContainsAny(s, "/%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '/':
			b.WriteString("%2f")
		case '%':
			b.WriteString("%25")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Unescape is the inverse of Escape. Invalid percent sequences are
// passed through verbatim.
func Unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			switch {
			case s[i+1] == '2' && (s[i+2] == 'f' || s[i+2] == 'F'):
				b.WriteByte('/')
				i += 3
				continue
			case s[i+1] == '2' && s[i+2] == '5':
				b.WriteByte('%')
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
