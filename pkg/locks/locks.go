// Package locks is the single cross-process write-coordination abstraction
// for yarilo. One Locker API, one TAB-delimited wire protocol, two backends
// behind it: embedded (in-memory, Unix socket) for standalone; remote
// (mTLS TCP, Redis-backed) for backend deployments. See https://doc.yarilomail.org/DEPLOYMENT
// §yarilo-locks for the design.
package locks

import (
	"context"
	"errors"
	"time"
)

// Lock represents a successfully acquired lock.
// Owners renew before ExpiresAt or the lock is reclaimed by the TTL sweeper.
type Lock struct {
	ID        string
	Resource  string
	Owner     string
	ExpiresAt time.Time
	// Site is what the holder was doing, on a BUSY answer. Reading it off the
	// refusal is the point: three questions in a row had to infer it (#1676).
	Site string
}

// Holder is who holds a lock and what they are doing with it. The two travel
// together because every question about a refusal needs both.
type Holder struct {
	Owner string
	Site  string
}

// EventType classifies an EVENT emitted by the server.
type EventType string

// Built-in event types. Callers may emit custom string types in addition.
const (
	EventChanged   EventType = "changed"   // mailbox contents changed
	EventDelivered EventType = "delivered" // new message arrived (LMTP)
	EventExpunged  EventType = "expunged"  // message removed

	// Mailbox-list events, published on MailboxListKey (per-user). They carry
	// the affected mailbox name(s) in the payload and drive NOTIFY (RFC 5465)
	// MailboxName / SubscriptionChange reporting to other sessions.
	EventMailboxCreate      EventType = "mbox-create"      // payload: name
	EventMailboxDelete      EventType = "mbox-delete"      // payload: name
	EventMailboxRename      EventType = "mbox-rename"      // payload: old\x00new
	EventMailboxSubscribe   EventType = "mbox-subscribe"   // payload: name
	EventMailboxUnsubscribe EventType = "mbox-unsubscribe" // payload: name
)

// Event is a server-emitted notification published on a resource channel.
type Event struct {
	Resource string
	Type     EventType
	Payload  string
}

// Locker is the single client-side API for cross-process write coordination.
// All session processes that mutate shared mailbox/index files go through
// this interface. Implementations talk the same TAB-delimited wire protocol
// over either a Unix socket (embedded) or mTLS TCP (remote).
type Locker interface {
	// Lock acquires an exclusive lock on resource for owner. ttl bounds the
	// hold time so a crashed owner cannot deadlock the system; callers must
	// Renew before expiry.
	//
	// Returns ErrBusy with the current owner captured in the returned Lock's
	// Owner field if the resource is already locked.
	Lock(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error)

	// LockShared acquires a shared (read) lock on resource for owner (#671).
	// Multiple owners may hold a shared lock on the same resource at once;
	// it only blocks against a concurrent exclusive Lock holder, and vice
	// versa — an exclusive Lock call blocks while any shared lock is held.
	//
	// Returns ErrBusy with the current exclusive owner if one is held.
	// Unlock/Renew work unchanged for a shared lock's ID.
	LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error)

	// Unlock releases a previously acquired lock by ID. Returns ErrNotFound
	// if the lock has already expired or been released.
	Unlock(ctx context.Context, lockID string) error

	// Renew extends the TTL of an existing lock. Returns ErrExpired if the
	// lock has already been reclaimed by the TTL sweeper.
	Renew(ctx context.Context, lockID string, ttl time.Duration) error

	// Subscribe returns a channel that receives every EVENT for resource.
	// The channel is closed when ctx is cancelled or the underlying
	// connection drops.
	Subscribe(ctx context.Context, resource string) (<-chan Event, error)

	// Emit publishes an event on resource. Used by writers (LMTP delivery,
	// IMAP APPEND/EXPUNGE) to notify IDLE subscribers.
	Emit(ctx context.Context, resource string, t EventType, payload string) error

	// HoldsResource reports whether this client currently holds an active
	// lock on resource. Storage backends consult it to skip re-acquiring
	// a lock they already hold via an outer scope — without this, a batch
	// operation (POP3 QUIT, IMAP-side multi-message Expunge) that takes
	// one outer X lock and then calls per-message storage methods would
	// deadlock on its own owner (yarilo-locks is non-reentrant by design).
	HoldsResource(resource string) bool

	// IncrementCounter atomically adds delta to the persistent counter
	// at key and returns the post-increment value. Used for shared
	// monotonic counters that must survive process restart and stay
	// race-free across pods — e.g. mdbox next_file_id allocation.
	//
	// Counters are independent of lock state: they share no key space
	// with Lock resources and are not affected by TTL sweeping.
	// First reference auto-initialises the counter to 0 before
	// applying delta.
	IncrementCounter(ctx context.Context, key string, delta int64) (int64, error)

	// Close terminates the underlying transport.
	Close() error
}

// Errors returned by Locker. Wrap with errors.Is at call sites.
var (
	// ErrBusy is returned by Lock when the resource is already held.
	// The returned Lock value carries the current owner in its Owner field.
	ErrBusy = errors.New("locks: resource is busy")

	// ErrNotFound is returned by Unlock when the lock ID is unknown
	// (already expired, released, or never existed).
	ErrNotFound = errors.New("locks: lock not found")

	// ErrExpired is returned by Renew when the lock has been reclaimed by
	// the TTL sweeper before the renewal arrived.
	ErrExpired = errors.New("locks: lock expired")

	// ErrClosed is returned when the client is used after Close.
	ErrClosed = errors.New("locks: client closed")

	// ErrProtocol indicates a malformed wire-protocol frame.
	ErrProtocol = errors.New("locks: protocol violation")

	// ErrUnavailable marks a failure to reach the lock service, as opposed to
	// a refusal by it. The distinction is what lets a protocol answer "try
	// again in a moment" instead of "this server is broken": a redeployed lock
	// service is temporary, and a client told otherwise stops retrying a
	// request that would work seconds later (#1339).
	ErrUnavailable = errors.New("locks: service unavailable")
)

// Backend is the server-side state interface. MemoryBackend (in-memory map)
// and RedisBackend (Redis Lua SET NX EX) implement it. Server is wire+I/O
// only — all state lives here.
type Backend interface {
	// Acquire attempts to take the exclusive lock. Returns the new lock ID on
	// success. On contention (an exclusive OR a shared holder already present),
	// returns ErrBusy with currentOwner populated.
	Acquire(ctx context.Context, resource, owner, site string, ttl time.Duration) (lockID string, current Holder, err error)

	// AcquireShared attempts to take a shared (read) lock (#671). Multiple
	// shared holders may coexist on the same resource; it fails with ErrBusy
	// only when an exclusive lock is currently held. Release/Renew use the
	// same lock-ID space as Acquire — implementations must track each lock
	// ID's kind (exclusive/shared) internally so those calls stay symmetric.
	AcquireShared(ctx context.Context, resource, owner, site string, ttl time.Duration) (lockID string, current Holder, err error)

	// Release deletes the lock. Returns ErrNotFound if the lock does not
	// exist anymore (expired or already released).
	Release(ctx context.Context, lockID string) error

	// Renew extends the TTL. Returns ErrExpired if the lock no longer exists.
	Renew(ctx context.Context, lockID string, ttl time.Duration) error

	// Publish broadcasts an event to all subscribers of resource.
	Publish(ctx context.Context, resource string, t EventType, payload string) error

	// Subscribe returns a channel for all events on resource. The returned
	// cancel function must be called to release backend resources.
	Subscribe(ctx context.Context, resource string) (<-chan Event, func(), error)

	// IncrementCounter atomically adds delta to the persistent counter
	// at key and returns the new value. Missing keys are auto-initialised
	// to 0 before applying delta. Counter state is independent of the
	// lock state: same backend, separate key namespace, no TTL.
	IncrementCounter(ctx context.Context, key string, delta int64) (int64, error)

	// Close shuts the backend down — closes Redis pool, stops sweepers, etc.
	Close() error
}
