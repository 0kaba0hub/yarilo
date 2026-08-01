package anvil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/internal/connlimit"
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

	// SessionConnect registers a session and enforces the per-user@IP limit
	// atomically. ok=true means registered; ok=false with err==nil means the
	// limit is reached (too-many-connections); err!=nil is a backend error — the
	// caller applies AnvilFailOpen, since a bounded Redis error is NOT a limit
	// rejection (#926/#932).
	SessionConnect(id, user, ip, service string) (ok bool, err error)
	// SessionDisconnect removes a session and frees its limit slot. Idempotent:
	// a second call (or one for an unknown id) must not drive the counter
	// negative.
	SessionDisconnect(id, user, ip string)
	// SessionTouch renews a session's liveness (heartbeat). Returns false when
	// the session is unknown (already reaped), so the caller can tell the pod to
	// re-register.
	SessionTouch(id string) (known bool)
	// SessionSetFolder / SessionSetBackend update session metadata; they return
	// false for an unknown id so the caller can reply "unknown".
	SessionSetFolder(id, folder string) (known bool)
	SessionSetBackend(id, backend string) (known bool)
	// SessionList returns a snapshot of all sessions.
	SessionList() []*SessionInfo
	// SessionLookupCount counts live sessions for (user, service); empty service
	// matches any.
	SessionLookupCount(user, service string) int
	// SessionCount returns the total number of tracked sessions.
	SessionCount() int

	// Maintain runs periodic upkeep: the memory backend sweeps stale sessions
	// and penalties; the Redis backend reconciles the connection counters from
	// live session keys (its penalties expire by TTL).
	Maintain(now time.Time)

	// Emit publishes payload to channel for delivery to current subscribers
	// (the kick bus, #908 PR3). Delivery is best-effort / at-most-once: an event
	// published while a subscriber is reconnecting is lost, and a full outbox is
	// dropped. This is NOT a correctness guarantee — the director's confirmed
	// ring-wide kill (#847) holds LOOKUP until the ring-wide session count is
	// stably zero, so split-writer safety never depends on a kick landing; the
	// kick only makes teardown prompt. See server.handleEmit.
	Emit(channel, payload string) error
	// Subscribe returns a channel of payloads for the named channel until ctx is
	// cancelled, at which point the returned channel is closed. The Redis backend
	// relays go-redis PubSub.Channel, which auto-reconnects and re-subscribes on a
	// Redis blip, so a transient Redis outage does not permanently deafen a
	// subscriber. The transport between a login pod and anvil is a separate
	// concern the caller must itself reconnect (login.kickSubscribeLoop).
	Subscribe(ctx context.Context, channel string) (<-chan string, error)

	// Close releases backend resources (the Redis client). Memory returns nil.
	Close() error
}

// subscriberOutboxSize bounds a subscriber's pending-event buffer. Emit drops
// events (best-effort) rather than blocking a publisher on a slow consumer.
const subscriberOutboxSize = 64

// penaltyEntry is the per-IP auth-fail counter for the memory backend. Sweep
// drops entries whose lastUpdate is older than the decay window.
type penaltyEntry struct {
	count      int
	lastUpdate time.Time
}

// memoryBackend is the in-process StateBackend — today's behaviour, unchanged.
type memoryBackend struct {
	decay      time.Duration
	sessionTTL time.Duration
	limiter    *connlimit.Limiter

	mu        sync.Mutex
	penalties map[string]*penaltyEntry
	sessions  map[string]*SessionInfo

	// subsMu guards subs, the in-process kick bus (#908 PR3): channel name →
	// subscriber outboxes. Non-blocking sends are done under this lock so a
	// concurrent unsubscribe (close) cannot race a send-on-closed-channel.
	subsMu sync.Mutex
	subs   map[string][]chan string
}

func newMemoryBackend(decay, sessionTTL time.Duration, max int) *memoryBackend {
	return &memoryBackend{
		decay:      decay,
		sessionTTL: sessionTTL,
		limiter:    connlimit.New(max),
		penalties:  make(map[string]*penaltyEntry),
		sessions:   make(map[string]*SessionInfo),
		subs:       make(map[string][]chan string),
	}
}

func (b *memoryBackend) SessionConnect(id, user, ip, service string) (bool, error) {
	now := time.Now().UTC()
	// Idempotent (#942 review): a retried CONNECT for an already-registered id
	// refreshes without taking a second limiter slot.
	b.mu.Lock()
	if sess, exists := b.sessions[id]; exists {
		sess.lastSeen = now
		b.mu.Unlock()
		return true, nil
	}
	b.mu.Unlock()

	if !b.limiter.Acquire(user, ip) {
		return false, nil
	}
	b.mu.Lock()
	if sess, exists := b.sessions[id]; exists {
		// Lost a race with a concurrent same-id CONNECT — undo the extra slot.
		sess.lastSeen = now
		b.mu.Unlock()
		b.limiter.Release(user, ip)
		return true, nil
	}
	b.sessions[id] = &SessionInfo{ID: id, User: user, IP: ip, Service: service, ConnectedAt: now, lastSeen: now}
	sessions.Set(float64(len(b.sessions)))
	b.mu.Unlock()
	return true, nil
}

func (b *memoryBackend) SessionDisconnect(id, user, ip string) {
	b.mu.Lock()
	_, existed := b.sessions[id]
	delete(b.sessions, id)
	sessions.Set(float64(len(b.sessions)))
	b.mu.Unlock()
	// Release only if the session existed, so a duplicate DISCONNECT cannot free
	// a slot twice — the same idempotency the Redis backend enforces via Lua.
	if existed {
		b.limiter.Release(user, ip)
	}
}

func (b *memoryBackend) SessionTouch(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[id]
	if ok {
		sess.lastSeen = time.Now().UTC()
	}
	return ok
}

func (b *memoryBackend) SessionSetFolder(id, folder string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[id]
	if ok {
		sess.Folder = folder
	}
	return ok
}

func (b *memoryBackend) SessionSetBackend(id, backend string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[id]
	if ok {
		sess.Backend = backend
	}
	return ok
}

func (b *memoryBackend) SessionList() []*SessionInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*SessionInfo, 0, len(b.sessions))
	for _, sess := range b.sessions {
		clone := *sess
		out = append(out, &clone)
	}
	return out
}

func (b *memoryBackend) SessionLookupCount(user, service string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, sess := range b.sessions {
		if sess.User != user {
			continue
		}
		if service != "" && !strings.EqualFold(sess.Service, service) {
			continue
		}
		count++
	}
	return count
}

func (b *memoryBackend) SessionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// Maintain sweeps stale sessions (releasing their limiter slots) and penalties —
// the historical sweeper behaviour, unchanged.
func (b *memoryBackend) Maintain(now time.Time) {
	cutoff := now.Add(-b.sessionTTL)
	type reap struct{ user, ip string }
	var dropped []reap
	b.mu.Lock()
	for id, sess := range b.sessions {
		if sess.lastSeen.Before(cutoff) {
			dropped = append(dropped, reap{user: sess.User, ip: sess.IP})
			delete(b.sessions, id)
		}
	}
	sessions.Set(float64(len(b.sessions)))
	b.mu.Unlock()
	sessionsReaped.Add(float64(len(dropped)))
	for _, r := range dropped {
		b.limiter.Release(r.user, r.ip)
	}
	b.PenaltySweep(now)
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

// Emit fans out to every subscriber on channel. Non-blocking: a full outbox is
// dropped (best-effort). The send happens under subsMu so it cannot race the
// close in Subscribe's cleanup goroutine.
func (b *memoryBackend) Emit(channel, payload string) error {
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	for _, box := range b.subs[channel] {
		select {
		case box <- payload:
		default:
		}
	}
	return nil
}

// Subscribe registers an outbox on channel and returns it. When ctx is
// cancelled the outbox is removed and closed, so the caller's range unwinds.
func (b *memoryBackend) Subscribe(ctx context.Context, channel string) (<-chan string, error) {
	out := make(chan string, subscriberOutboxSize)
	b.subsMu.Lock()
	b.subs[channel] = append(b.subs[channel], out)
	b.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		b.subsMu.Lock()
		defer b.subsMu.Unlock()
		list := b.subs[channel]
		for i, ch := range list {
			if ch == out {
				b.subs[channel] = append(list[:i], list[i+1:]...)
				break
			}
		}
		close(out)
	}()
	return out, nil
}

func (b *memoryBackend) Close() error { return nil }

// redisOpTimeout bounds every Redis operation so a blackholed or down Redis
// fails fast instead of hanging a handler — the #926/#932 lesson. A read error
// is treated as "no penalty" (fail-open), matching the connection-limit
// fail-open posture; the caller's AnvilFailOpen governs the session decision.
const redisOpTimeout = 3 * time.Second

// scanCount is the COUNT hint for SCAN — cursor-based, non-blocking iteration
// (never KEYS), so listing/reconciliation stays cheap even as sessions grow.
const scanCount = 256

// unixToTime parses a stored unix-seconds string; zero on any parse error.
func unixToTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// redisBackend is the Redis-backed StateBackend. In this PR it implements only
// the penalty counter (SET EX / GET / DEL); sessions and the kick bus land in
// later phases. The caller owns the client lifecycle beyond Close.
type redisBackend struct {
	rdb           *redis.Client
	keyPrefix     string
	channelPrefix string
	penaltyTTL    time.Duration
	sessionTTL    time.Duration
	limit         int
}

// NewRedisBackend wraps a client as a Redis StateBackend. keyPrefix namespaces
// every key and channelPrefix every Pub/Sub channel (#938/#939 practice);
// penaltyTTL is the penalty decay window and sessionTTL the per-session key TTL
// (renewed by heartbeat), both enforced by Redis key expiry; limit is the
// per-user@IP connection cap (0 = unlimited). The caller owns the client
// lifecycle beyond Close.
func NewRedisBackend(rdb *redis.Client, keyPrefix, channelPrefix string, penaltyTTL, sessionTTL time.Duration, limit int) StateBackend {
	return &redisBackend{rdb: rdb, keyPrefix: keyPrefix, channelPrefix: channelPrefix, penaltyTTL: penaltyTTL, sessionTTL: sessionTTL, limit: limit}
}

func (b *redisBackend) penaltyKey(ip string) string   { return b.keyPrefix + "penalty:" + ip }
func (b *redisBackend) cntKey(user, ip string) string { return b.keyPrefix + "cnt:" + user + "@" + ip }
func (b *redisBackend) sessKey(id string) string      { return b.keyPrefix + "sess:" + id }
func (b *redisBackend) chanKey(channel string) string { return b.channelPrefix + channel }

// connectScript is the single point of atomicity for CONNECT (mirrors
// locks.Acquire): check the counter against the limit, and only on pass create
// the session hash (with TTL) and increment the counter — all in one round trip,
// so concurrent CONNECTs cannot both slip past the limit.
// KEYS[1]=cnt KEYS[2]=sess; ARGV: 1=limit 2=ttlMs 3=user 4=ip 5=service 6=connectedAt
var connectScript = redis.NewScript(`
-- Idempotent (#942 review): a retried CONNECT for an already-registered session
-- (a network retry after the command actually succeeded) must refresh it WITHOUT
-- a second INCR, or one session would be double-counted. Mirrors the idempotent
-- DISCONNECT (which DECRs only on DEL==1).
if redis.call('EXISTS', KEYS[2]) == 1 then
  redis.call('HSET', KEYS[2], 'user', ARGV[3], 'ip', ARGV[4], 'service', ARGV[5], 'connected_at', ARGV[6])
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
  return 1
end
local limit = tonumber(ARGV[1])
if limit > 0 and tonumber(redis.call('GET', KEYS[1]) or '0') >= limit then
  return 0
end
redis.call('HSET', KEYS[2], 'user', ARGV[3], 'ip', ARGV[4], 'service', ARGV[5], 'connected_at', ARGV[6])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
redis.call('INCR', KEYS[1])
return 1
`)

// disconnectScript is the mirrored idempotent release: decrement the counter
// ONLY if the session key still existed, so a duplicate DISCONNECT — or one for
// a session already TTL-expired after a crash — cannot drive the counter
// negative. KEYS[1]=cnt KEYS[2]=sess
var disconnectScript = redis.NewScript(`
if redis.call('DEL', KEYS[2]) == 1 then
  if redis.call('DECR', KEYS[1]) <= 0 then redis.call('DEL', KEYS[1]) end
end
return 1
`)

// setFieldScript updates one session-hash field only if the session still
// exists, so a late SELECT/BACKEND cannot recreate a TTL-less hash (a leak).
// KEYS[1]=sess; ARGV: 1=field 2=value
var setFieldScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
  return 1
end
return 0
`)

// reconcileScript corrects a leaked counter race-safely: DECRBY by the leak
// observed at scan time, NEVER a blind SET. Concurrent CONNECT and DISCONNECT
// move the counter and the live-session tally EQUALLY, so their difference (the
// leak) is invariant between the scan and this decrement — it lands on the right
// value whatever raced in between. KEYS[1]=cnt; ARGV[1]=leak(>0)
var reconcileScript = redis.NewScript(`
if redis.call('DECRBY', KEYS[1], tonumber(ARGV[1])) <= 0 then redis.call('DEL', KEYS[1]) end
return 1
`)

func (b *redisBackend) PenaltyLookup(ip string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	v, err := b.rdb.Get(ctx, b.penaltyKey(ip)).Int()
	switch {
	case err == nil:
		return v, "hit"
	case errors.Is(err, redis.Nil):
		return 0, "miss" // key absent or expired — a real "no penalty"
	default:
		// A bounded Redis error (down/blackholed). Fail open — a tarpit is an
		// availability control, and failing closed would tarpit everyone during a
		// Redis blip (self-DoS). The distinct "error" status keeps the outage
		// visible in the penaltyLookups metric rather than hiding it as a miss.
		return 0, "error"
	}
}

func (b *redisBackend) PenaltyUpdate(ip string, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	key := b.penaltyKey(ip)
	if count <= 0 {
		_ = b.rdb.Del(ctx, key).Err()
		return
	}
	_ = b.rdb.Set(ctx, key, strconv.Itoa(count), b.penaltyTTL).Err()
}

// PenaltySweep is a no-op: Redis key TTL evicts stale penalties.
func (b *redisBackend) PenaltySweep(time.Time) {}

func (b *redisBackend) SessionConnect(id, user, ip, service string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	res, err := connectScript.Run(ctx, b.rdb,
		[]string{b.cntKey(user, ip), b.sessKey(id)},
		b.limit, b.sessionTTL.Milliseconds(), user, ip, service, strconv.FormatInt(time.Now().UTC().Unix(), 10),
	).Int()
	if err != nil {
		return false, err // bounded backend error → caller applies AnvilFailOpen
	}
	return res == 1, nil
}

func (b *redisBackend) SessionDisconnect(id, user, ip string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	_ = disconnectScript.Run(ctx, b.rdb, []string{b.cntKey(user, ip), b.sessKey(id)}).Err()
}

func (b *redisBackend) SessionTouch(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	ok, err := b.rdb.PExpire(ctx, b.sessKey(id), b.sessionTTL).Result()
	if err != nil {
		return false
	}
	return ok
}

func (b *redisBackend) SessionSetFolder(id, folder string) bool {
	return b.setField(id, "folder", folder)
}
func (b *redisBackend) SessionSetBackend(id, backend string) bool {
	return b.setField(id, "backend", backend)
}

func (b *redisBackend) setField(id, field, value string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	res, err := setFieldScript.Run(ctx, b.rdb, []string{b.sessKey(id)}, field, value).Int()
	return err == nil && res == 1
}

func (b *redisBackend) SessionList() []*SessionInfo {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	out := []*SessionInfo{}
	iter := b.rdb.Scan(ctx, 0, b.keyPrefix+"sess:*", scanCount).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		h, err := b.rdb.HGetAll(ctx, key).Result()
		if err != nil || len(h) == 0 {
			continue // raced with an expiry/DEL
		}
		out = append(out, &SessionInfo{
			ID:          strings.TrimPrefix(key, b.keyPrefix+"sess:"),
			User:        h["user"],
			IP:          h["ip"],
			Service:     h["service"],
			Folder:      h["folder"],
			Backend:     h["backend"],
			ConnectedAt: unixToTime(h["connected_at"]),
		})
	}
	return out
}

func (b *redisBackend) SessionLookupCount(user, service string) int {
	count := 0
	for _, s := range b.SessionList() {
		if s.User != user {
			continue
		}
		if service != "" && !strings.EqualFold(s.Service, service) {
			continue
		}
		count++
	}
	return count
}

func (b *redisBackend) SessionCount() int { return len(b.SessionList()) }

// Maintain reconciles leaked connection counters (#908): sessions of a crashed
// login pod expire by TTL but their INCR was never matched by a DECR, so a
// counter can drift high. Tally the live sessions per user@IP, then DECRBY each
// counter by its leak (scanned counter − live tally) via reconcileScript, which
// is race-safe against concurrent CONNECT/DISCONNECT. Penalties expire by TTL,
// so there is nothing to sweep for them.
func (b *redisBackend) Maintain(time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*redisOpTimeout)
	defer cancel()

	// Live tally per user@IP from the session hashes.
	live := map[string]int{}
	sit := b.rdb.Scan(ctx, 0, b.keyPrefix+"sess:*", scanCount).Iterator()
	for sit.Next(ctx) {
		h, err := b.rdb.HMGet(ctx, sit.Val(), "user", "ip").Result()
		if err != nil || len(h) != 2 {
			continue
		}
		u, ok0 := h[0].(string)
		ipv, ok1 := h[1].(string)
		if !ok0 || !ok1 {
			continue // missing/expired field, or a non-string value
		}
		live[u+"@"+ipv]++
	}

	// Compare each counter to the tally; decrement any leak.
	cit := b.rdb.Scan(ctx, 0, b.keyPrefix+"cnt:*", scanCount).Iterator()
	for cit.Next(ctx) {
		key := cit.Val()
		cur, err := b.rdb.Get(ctx, key).Int()
		if err != nil {
			continue
		}
		userip := strings.TrimPrefix(key, b.keyPrefix+"cnt:")
		if leak := cur - live[userip]; leak > 0 {
			_ = reconcileScript.Run(ctx, b.rdb, []string{key}, leak).Err()
		}
	}
}

// Emit PUBLISHes payload on the prefixed channel. Bounded by redisOpTimeout so a
// down Redis fails fast rather than stalling the publisher.
func (b *redisBackend) Emit(channel, payload string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := b.rdb.Publish(ctx, b.chanKey(channel), payload).Err(); err != nil {
		return fmt.Errorf("anvil/redis: publish %s: %w", channel, err)
	}
	return nil
}

// Subscribe relays go-redis PubSub.Channel for the prefixed channel. Channel()
// owns reconnection: on a dropped Redis connection it redials and re-subscribes
// transparently, so a Redis blip does not permanently deafen the subscriber
// (#908 PR3 requirement — the mirror of #946 on the subscribe side). The relay
// goroutine ends, closing out and the PubSub, only when ctx is cancelled.
func (b *redisBackend) Subscribe(ctx context.Context, channel string) (<-chan string, error) {
	ps := b.rdb.Subscribe(ctx, b.chanKey(channel))
	// Confirm the subscription is established so a dead Redis surfaces here
	// rather than as silent non-delivery later.
	rctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	if _, err := ps.Receive(rctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("anvil/redis: subscribe %s: %w", channel, err)
	}
	out := make(chan string, subscriberOutboxSize)
	go func() {
		defer close(out)
		defer func() { _ = ps.Close() }()
		rc := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-rc:
				if !ok {
					return
				}
				select {
				case out <- msg.Payload:
				default:
					// Slow consumer — drop (best-effort, matches memory).
				}
			}
		}
	}()
	return out, nil
}

func (b *redisBackend) Close() error { return b.rdb.Close() }

// WithStateBackend overrides the default in-memory state store (#908). Used to
// inject the Redis backend in k8s and a stub in tests.
func WithStateBackend(sb StateBackend) ServerOption {
	return func(s *Server) { s.state = sb }
}
