package anvil

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// StateBackend is anvil's pluggable shared-state store (#908). The memory
// backend is the historical in-process behaviour and the default for standalone
// and tests; the Redis backend lets state survive a pod restart and be shared
// across replicas. This PR (1) covers the per-IP auth-failure penalty counter
// only; session accounting (PR2, Lua check-and-increment) and the kick bus (PR3,
// Redis Pub/Sub) extend this interface in later phases.
type StateBackend interface {
	// PenaltyLookup returns the current auth-failure count for ip and a status
	// for metrics: "hit", "miss", or "expired". Redis cannot distinguish an
	// expired key from one that never existed and reports "miss" for both.
	PenaltyLookup(ip string) (count int, status string)
	// PenaltyUpdate sets the count for ip; count <= 0 clears the entry.
	PenaltyUpdate(ip string, count int)
	// PenaltySweep drops entries older than the decay window. Memory-only; the
	// Redis backend relies on key TTL and no-ops.
	PenaltySweep(now time.Time)
	// Close releases backend resources (the Redis client). Memory returns nil.
	Close() error
}

// penaltyEntry is the per-IP auth-fail counter for the memory backend. Sweep
// drops entries whose lastUpdate is older than the decay window.
type penaltyEntry struct {
	count      int
	lastUpdate time.Time
}

// memoryBackend is the in-process StateBackend — today's behaviour, unchanged.
type memoryBackend struct {
	decay time.Duration

	mu        sync.Mutex
	penalties map[string]*penaltyEntry
}

func newMemoryBackend(decay time.Duration) *memoryBackend {
	return &memoryBackend{decay: decay, penalties: make(map[string]*penaltyEntry)}
}

func (b *memoryBackend) PenaltyLookup(ip string) (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.penalties[ip]
	if !ok {
		return 0, "miss"
	}
	if time.Since(e.lastUpdate) > b.decay {
		delete(b.penalties, ip) // lazy eviction on read
		return 0, "expired"
	}
	return e.count, "hit"
}

func (b *memoryBackend) PenaltyUpdate(ip string, count int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if count <= 0 {
		delete(b.penalties, ip)
		return
	}
	b.penalties[ip] = &penaltyEntry{count: count, lastUpdate: time.Now().UTC()}
}

func (b *memoryBackend) PenaltySweep(now time.Time) {
	cutoff := now.Add(-b.decay)
	b.mu.Lock()
	defer b.mu.Unlock()
	for ip, e := range b.penalties {
		if e.lastUpdate.Before(cutoff) {
			delete(b.penalties, ip)
		}
	}
}

func (b *memoryBackend) Close() error { return nil }

// redisOpTimeout bounds every Redis operation so a blackholed or down Redis
// fails fast instead of hanging a handler — the #926/#932 lesson. A read error
// is treated as "no penalty" (fail-open), matching the connection-limit
// fail-open posture; the caller's AnvilFailOpen governs the session decision.
const redisOpTimeout = 3 * time.Second

// redisBackend is the Redis-backed StateBackend. In this PR it implements only
// the penalty counter (SET EX / GET / DEL); sessions and the kick bus land in
// later phases. The caller owns the client lifecycle beyond Close.
type redisBackend struct {
	rdb       *redis.Client
	keyPrefix string
	ttl       time.Duration
}

// NewRedisBackend wraps a client as a Redis StateBackend. keyPrefix namespaces
// every key (#938/#939 practice); ttl is the penalty decay window, enforced by
// Redis key expiry. The caller owns the client lifecycle beyond Close.
func NewRedisBackend(rdb *redis.Client, keyPrefix string, ttl time.Duration) StateBackend {
	return &redisBackend{rdb: rdb, keyPrefix: keyPrefix, ttl: ttl}
}

func (b *redisBackend) penaltyKey(ip string) string { return b.keyPrefix + "penalty:" + ip }

func (b *redisBackend) PenaltyLookup(ip string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	v, err := b.rdb.Get(ctx, b.penaltyKey(ip)).Int()
	if err != nil {
		return 0, "miss" // redis.Nil (absent/expired) or a bounded error → fail open
	}
	return v, "hit"
}

func (b *redisBackend) PenaltyUpdate(ip string, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	key := b.penaltyKey(ip)
	if count <= 0 {
		_ = b.rdb.Del(ctx, key).Err()
		return
	}
	_ = b.rdb.Set(ctx, key, strconv.Itoa(count), b.ttl).Err()
}

// PenaltySweep is a no-op: Redis key TTL evicts stale penalties.
func (b *redisBackend) PenaltySweep(time.Time) {}

func (b *redisBackend) Close() error { return b.rdb.Close() }

// WithStateBackend overrides the default in-memory state store (#908). Used to
// inject the Redis backend in k8s and a stub in tests.
func WithStateBackend(sb StateBackend) ServerOption {
	return func(s *Server) { s.state = sb }
}
