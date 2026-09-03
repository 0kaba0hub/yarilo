package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// MemoryBackend is the in-memory state backend used by embedded mode
// (standalone deployments). State is ephemeral: on process crash all locks
// are lost, which is acceptable for standalone where every session process
// restarts with the pod.
type MemoryBackend struct {
	mu        sync.Mutex
	locks     map[string]*memLock                // by lockID, exclusive AND shared
	byRes     map[string]string                  // resource → lockID, exclusive holder only
	sharedRes map[string]map[string]struct{}     // resource → set of shared lockIDs
	subs      map[string]map[chan Event]struct{} // resource → subscribers
	counters  map[string]int64                   // persistent atomic counters by key
	sweepInt  time.Duration
	now       func() time.Time
	stopOnce  sync.Once
	stop      chan struct{}
}

type memLock struct {
	ID        string
	Resource  string
	Owner     string
	ExpiresAt time.Time
	Shared    bool
}

// MemoryBackendOption tunes the backend at construction time.
type MemoryBackendOption func(*MemoryBackend)

// WithSweepInterval overrides the default TTL sweep cadence (100 ms).
// Tests typically override this to a short value (1 ms) so expirations land
// inside test deadlines.
func WithSweepInterval(d time.Duration) MemoryBackendOption {
	return func(b *MemoryBackend) { b.sweepInt = d }
}

// WithNow overrides the time source. Tests use this to drive deterministic
// expirations without sleeping.
func WithNow(now func() time.Time) MemoryBackendOption {
	return func(b *MemoryBackend) { b.now = now }
}

// NewMemoryBackend returns a fresh in-memory backend and starts its
// background TTL sweeper. Call Close to stop the sweeper and release
// subscriber goroutines.
func NewMemoryBackend(opts ...MemoryBackendOption) *MemoryBackend {
	b := &MemoryBackend{
		locks:     make(map[string]*memLock),
		byRes:     make(map[string]string),
		sharedRes: make(map[string]map[string]struct{}),
		subs:      make(map[string]map[chan Event]struct{}),
		counters:  make(map[string]int64),
		sweepInt:  100 * time.Millisecond,
		now:       time.Now,
		stop:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	go b.sweepLoop()
	return b
}

// Acquire implements Backend. Fails if the resource has an exclusive
// holder OR any shared holder — exclusive is exclusive against everyone.
func (b *MemoryBackend) Acquire(_ context.Context, resource, owner string, ttl time.Duration) (string, string, error) {
	if resource == "" || owner == "" {
		return "", "", fmt.Errorf("locks/memory: resource and owner must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if existing, held := b.byRes[resource]; held {
		return "", b.locks[existing].Owner, ErrBusy
	}
	if shared := b.sharedRes[resource]; len(shared) > 0 {
		for id := range shared {
			who := b.locks[id].Owner
			if n := len(shared) - 1; n > 0 {
				who = fmt.Sprintf("%s +%d", who, n)
			}
			return "", who, ErrBusy
		}
	}
	id, err := randID()
	if err != nil {
		return "", "", fmt.Errorf("locks/memory: generate id: %w", err)
	}
	b.locks[id] = &memLock{
		ID:        id,
		Resource:  resource,
		Owner:     owner,
		ExpiresAt: b.now().Add(ttl),
	}
	b.byRes[resource] = id
	return id, "", nil
}

// AcquireShared implements Backend. Multiple shared holders may coexist on
// the same resource; only an exclusive holder blocks it.
func (b *MemoryBackend) AcquireShared(_ context.Context, resource, owner string, ttl time.Duration) (string, string, error) {
	if resource == "" || owner == "" {
		return "", "", fmt.Errorf("locks/memory: resource and owner must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if existing, held := b.byRes[resource]; held {
		return "", b.locks[existing].Owner, ErrBusy
	}
	id, err := randID()
	if err != nil {
		return "", "", fmt.Errorf("locks/memory: generate id: %w", err)
	}
	b.locks[id] = &memLock{
		ID:        id,
		Resource:  resource,
		Owner:     owner,
		ExpiresAt: b.now().Add(ttl),
		Shared:    true,
	}
	if b.sharedRes[resource] == nil {
		b.sharedRes[resource] = make(map[string]struct{})
	}
	b.sharedRes[resource][id] = struct{}{}
	return id, "", nil
}

// Release implements Backend.
func (b *MemoryBackend) Release(_ context.Context, lockID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	l, ok := b.locks[lockID]
	if !ok {
		return ErrNotFound
	}
	delete(b.locks, lockID)
	b.releaseFromIndexLocked(l)
	return nil
}

// releaseFromIndexLocked removes l from whichever secondary index
// (byRes or sharedRes) tracks its kind. Caller holds b.mu.
func (b *MemoryBackend) releaseFromIndexLocked(l *memLock) {
	if l.Shared {
		if set, ok := b.sharedRes[l.Resource]; ok {
			delete(set, l.ID)
			if len(set) == 0 {
				delete(b.sharedRes, l.Resource)
			}
		}
		return
	}
	delete(b.byRes, l.Resource)
}

// Renew implements Backend.
func (b *MemoryBackend) Renew(_ context.Context, lockID string, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	l, ok := b.locks[lockID]
	if !ok {
		return ErrExpired
	}
	l.ExpiresAt = b.now().Add(ttl)
	return nil
}

// Publish implements Backend.
func (b *MemoryBackend) Publish(_ context.Context, resource string, t EventType, payload string) error {
	b.mu.Lock()
	subs := b.subs[resource]
	chans := make([]chan Event, 0, len(subs))
	for ch := range subs {
		chans = append(chans, ch)
	}
	b.mu.Unlock()
	evt := Event{Resource: resource, Type: t, Payload: payload}
	for _, ch := range chans {
		// Non-blocking send — a slow subscriber must not stall publishers.
		// Dropped events are acceptable for IDLE notifications: the client
		// re-syncs from index state on the next user command.
		select {
		case ch <- evt:
		default:
		}
	}
	return nil
}

// Subscribe implements Backend.
func (b *MemoryBackend) Subscribe(_ context.Context, resource string) (<-chan Event, func(), error) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	if b.subs[resource] == nil {
		b.subs[resource] = make(map[chan Event]struct{})
	}
	b.subs[resource][ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if set, ok := b.subs[resource]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, resource)
			}
		}
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel, nil
}

// IncrementCounter implements Backend. Auto-initialises missing
// keys to 0 before applying delta. Counter state is independent
// of the lock state and is not touched by the TTL sweeper.
func (b *MemoryBackend) IncrementCounter(_ context.Context, key string, delta int64) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("locks/memory: counter key must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters[key] += delta
	return b.counters[key], nil
}

// Close implements Backend. Idempotent.
func (b *MemoryBackend) Close() error {
	b.stopOnce.Do(func() { close(b.stop) })
	return nil
}

func (b *MemoryBackend) sweepLoop() {
	t := time.NewTicker(b.sweepInt)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.mu.Lock()
			b.expireLocked()
			b.mu.Unlock()
		case <-b.stop:
			return
		}
	}
}

// expireLocked removes locks whose TTL has elapsed. Caller holds b.mu.
func (b *MemoryBackend) expireLocked() {
	now := b.now()
	for id, l := range b.locks {
		if !now.Before(l.ExpiresAt) {
			delete(b.locks, id)
			b.releaseFromIndexLocked(l)
		}
	}
}

// randID returns a 16-byte hex random identifier.
func randID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
