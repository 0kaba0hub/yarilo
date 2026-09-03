package locks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisBackend stores lock state in Redis. Used by remote mode in backend
// deployments where multiple yarilo-locks replicas must share a state view
// for HA. Atomic acquisition via SET NX EX, atomic ownership-checked
// release/renew via Lua to avoid lost-update races between concurrent owners.
type RedisBackend struct {
	rdb       redis.UniversalClient
	keyPrefix string
	chPrefix  string
}

// RedisOption tunes the backend at construction time.
type RedisOption func(*RedisBackend)

// WithKeyPrefix overrides the Redis key prefix used for lock state.
// Default: "yarilo:locks:". Keys take the form "<prefix><lockID>".
func WithKeyPrefix(p string) RedisOption {
	return func(b *RedisBackend) { b.keyPrefix = p }
}

// WithChannelPrefix overrides the Redis channel prefix used for events.
// Default: "yarilo:events:". Channels take the form "<prefix><resource>".
func WithChannelPrefix(p string) RedisOption {
	return func(b *RedisBackend) { b.chPrefix = p }
}

// NewRedisBackend wraps a redis client. The caller owns the client lifecycle
// outside of Close, which closes the wrapped client.
func NewRedisBackend(rdb redis.UniversalClient, opts ...RedisOption) *RedisBackend {
	b := &RedisBackend{
		rdb:       rdb,
		keyPrefix: "yarilo:locks:",
		chPrefix:  "yarilo:events:",
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// lockValue is the stored value: "<resource>|<owner>|<kind>", kind ∈
// {"x","s"} (exclusive/shared). Used to enforce owner-checked release/renew
// (an owner cannot release another owner's lock that happened to take the
// same ID — even though IDs are random 16-byte, this defends against ID
// forgery in a multi-tenant deployment), and to route Release/Renew to the
// correct secondary index (resKey vs sharedKey) without the caller having to
// track kind itself. The reverse split is performed server-side in the Lua
// scripts (string.find on '|').
func lockValue(resource, owner, kind string) string { return resource + "|" + owner + "|" + kind }

// resKey for the secondary index "resource → lockID" — the exclusive holder,
// at most one per resource at a time. Stored as a sibling key with the same TTL.
func (b *RedisBackend) resKey(resource string) string {
	return b.keyPrefix + "res:" + resource
}

// sharedKey for the secondary index of shared holders on resource: a ZSET
// with member=lockID, score=expiry_ms — lets AcquireExclusive atomically
// prune expired shared holders (ZREMRANGEBYSCORE) and check ZCARD without a
// per-member TTL sweeper.
func (b *RedisBackend) sharedKey(resource string) string {
	return b.keyPrefix + "shared:" + resource
}

func (b *RedisBackend) lockKey(lockID string) string {
	return b.keyPrefix + "id:" + lockID
}

func (b *RedisBackend) channel(resource string) string {
	return b.chPrefix + resource
}

// luaParseOwnerAndKind is shared Lua source (inlined into each script, Redis
// scripts cannot cross-call each other) that splits a "resource|owner|kind"
// lockValue into its three parts.
const luaParseValue = `
local function parseValue(v)
  local sep1 = string.find(v, "|", 1, true)
  if not sep1 then return nil, nil, nil end
  local resource = string.sub(v, 1, sep1 - 1)
  local rest = string.sub(v, sep1 + 1)
  local sep2 = string.find(rest, "|", 1, true)
  if not sep2 then return resource, rest, "x" end
  local owner = string.sub(rest, 1, sep2 - 1)
  local kind = string.sub(rest, sep2 + 1)
  return resource, owner, kind
end
local function nowMs()
  local t = redis.call("TIME")
  return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
`

// acquireScript takes the exclusive lock: it fails if either the resource
// index key (another exclusive holder) or the resource's shared ZSET
// (any live shared holder) is non-empty, pruning expired shared members
// first so a stale, never-released entry cannot wedge every future
// exclusive Acquire.
//
//	KEYS[1] = resource index key   (yarilo:locks:res:<resource>)
//	KEYS[2] = lock id key          (yarilo:locks:id:<lockID>)
//	KEYS[3] = shared ZSET key      (yarilo:locks:shared:<resource>)
//	ARGV[1] = lockID
//	ARGV[2] = lockValue (resource|owner|x)
//	ARGV[3] = ttl_ms
//	ARGV[4] = key prefix (to reach a shared holder's id key)
//
//	returns {"OK"} on success, or {"BUSY", <current_owner>} on contention.
var acquireScript = redis.NewScript(luaParseValue + `
local existing = redis.call("GET", KEYS[1])
if existing then
  local v = redis.call("GET", KEYS[1] .. ":val")
  local owner = ""
  if v then local _, o = parseValue(v); owner = o end
  return {"BUSY", owner or ""}
end
redis.call("ZREMRANGEBYSCORE", KEYS[3], "-inf", nowMs())
local sharedCount = redis.call("ZCARD", KEYS[3])
if sharedCount > 0 then
  -- Name one of the readers and say how many there are. The members are in
  -- hand, and a refusal that names nobody was 17% of them (#1652).
  local first = redis.call("ZRANGE", KEYS[3], 0, 0)
  local who = ""
  if first[1] then
    local v = redis.call("GET", ARGV[4] .. "id:" .. first[1])
    if v then local _, o = parseValue(v); who = o or "" end
  end
  if sharedCount > 1 then
    who = who .. " +" .. tostring(sharedCount - 1)
  end
  return {"BUSY", who}
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[3])
redis.call("SET", KEYS[1] .. ":val", ARGV[2], "PX", ARGV[3])
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
return {"OK"}
`)

// acquireSharedScript takes a shared lock: it only fails if the resource's
// exclusive holder key is present. Any number of shared holders may coexist,
// tracked as ZSET members (score = expiry_ms) so exclusive Acquire can prune
// and count them without a per-member TTL sweeper.
//
//	KEYS[1] = resource index key
//	KEYS[2] = lock id key
//	KEYS[3] = shared ZSET key
//	ARGV[1] = lockID
//	ARGV[2] = lockValue (resource|owner|s)
//	ARGV[3] = ttl_ms
//	ARGV[4] = key prefix (to reach a shared holder's id key)
//
//	returns {"OK"} on success, or {"BUSY", <current_owner>} on contention.
var acquireSharedScript = redis.NewScript(luaParseValue + `
local existing = redis.call("GET", KEYS[1])
if existing then
  local v = redis.call("GET", KEYS[1] .. ":val")
  local owner = ""
  if v then local _, o = parseValue(v); owner = o end
  return {"BUSY", owner or ""}
end
redis.call("ZADD", KEYS[3], nowMs() + tonumber(ARGV[3]), ARGV[1])
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
return {"OK"}
`)

// releaseScript deletes the lock by ID, routing to the exclusive resource
// index or the shared ZSET depending on the lockValue's kind field,
// validating that the resource-index still points at the same ID for the
// exclusive case.
//
//	KEYS[1] = lock id key
//	ARGV[1] = lockID
//	ARGV[2] = key prefix (to build resource/shared index keys from lockValue)
//
//	returns 1 on success, 0 if not found.
var releaseScript = redis.NewScript(luaParseValue + `
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
local resource, _, kind = parseValue(v)
if not resource then return 0 end
if kind == "s" then
  redis.call("ZREM", ARGV[2] .. "shared:" .. resource, ARGV[1])
else
  local resKey = ARGV[2] .. "res:" .. resource
  local current = redis.call("GET", resKey)
  if current == ARGV[1] then
    redis.call("DEL", resKey)
    redis.call("DEL", resKey .. ":val")
  end
end
redis.call("DEL", KEYS[1])
return 1
`)

// renewScript extends the TTL, routing to the exclusive resource index or
// the shared ZSET member's score depending on the lockValue's kind field.
//
//	KEYS[1] = lock id key
//	ARGV[1] = lockID
//	ARGV[2] = ttl_ms
//	ARGV[3] = key prefix
//
//	returns 1 on success, 0 if expired.
var renewScript = redis.NewScript(luaParseValue + `
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
local resource, _, kind = parseValue(v)
if not resource then return 0 end
if kind == "s" then
  local sharedKey = ARGV[3] .. "shared:" .. resource
  local score = redis.call("ZSCORE", sharedKey, ARGV[1])
  if not score then return 0 end
  redis.call("ZADD", sharedKey, nowMs() + tonumber(ARGV[2]), ARGV[1])
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return 1
end
local resKey = ARGV[3] .. "res:" .. resource
local current = redis.call("GET", resKey)
if current ~= ARGV[1] then return 0 end
redis.call("PEXPIRE", resKey, ARGV[2])
redis.call("PEXPIRE", resKey .. ":val", ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[2])
return 1
`)

// Acquire implements Backend.
func (b *RedisBackend) Acquire(ctx context.Context, resource, owner string, ttl time.Duration) (string, string, error) {
	if resource == "" || owner == "" {
		return "", "", fmt.Errorf("locks/redis: resource and owner must be non-empty")
	}
	id, err := randID()
	if err != nil {
		return "", "", fmt.Errorf("locks/redis: generate id: %w", err)
	}
	res, err := acquireScript.Run(ctx, b.rdb,
		[]string{b.resKey(resource), b.lockKey(id), b.sharedKey(resource)},
		id, lockValue(resource, owner, "x"), ttl.Milliseconds(), b.keyPrefix,
	).Result()
	if err != nil {
		return "", "", fmt.Errorf("locks/redis: acquire: %w", err)
	}
	return parseAcquireResult(res, id)
}

// AcquireShared implements Backend.
func (b *RedisBackend) AcquireShared(ctx context.Context, resource, owner string, ttl time.Duration) (string, string, error) {
	if resource == "" || owner == "" {
		return "", "", fmt.Errorf("locks/redis: resource and owner must be non-empty")
	}
	id, err := randID()
	if err != nil {
		return "", "", fmt.Errorf("locks/redis: generate id: %w", err)
	}
	res, err := acquireSharedScript.Run(ctx, b.rdb,
		[]string{b.resKey(resource), b.lockKey(id), b.sharedKey(resource)},
		id, lockValue(resource, owner, "s"), ttl.Milliseconds(),
	).Result()
	if err != nil {
		return "", "", fmt.Errorf("locks/redis: acquire shared: %w", err)
	}
	return parseAcquireResult(res, id)
}

func parseAcquireResult(res interface{}, id string) (string, string, error) {
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		return "", "", fmt.Errorf("locks/redis: malformed acquire response: %w", ErrProtocol)
	}
	switch arr[0] {
	case "OK":
		return id, "", nil
	case "BUSY":
		current := ""
		if len(arr) > 1 {
			current, _ = arr[1].(string)
		}
		return "", current, ErrBusy
	}
	return "", "", fmt.Errorf("locks/redis: unexpected acquire response %v", arr)
}

// Release implements Backend.
func (b *RedisBackend) Release(ctx context.Context, lockID string) error {
	res, err := releaseScript.Run(ctx, b.rdb,
		[]string{b.lockKey(lockID)},
		lockID, b.keyPrefix,
	).Result()
	if err != nil {
		return fmt.Errorf("locks/redis: release: %w", err)
	}
	if n, _ := res.(int64); n != 1 {
		return ErrNotFound
	}
	return nil
}

// Renew implements Backend.
func (b *RedisBackend) Renew(ctx context.Context, lockID string, ttl time.Duration) error {
	res, err := renewScript.Run(ctx, b.rdb,
		[]string{b.lockKey(lockID)},
		lockID, ttl.Milliseconds(), b.keyPrefix,
	).Result()
	if err != nil {
		return fmt.Errorf("locks/redis: renew: %w", err)
	}
	if n, _ := res.(int64); n != 1 {
		return ErrExpired
	}
	return nil
}

// Publish implements Backend.
func (b *RedisBackend) Publish(ctx context.Context, resource string, t EventType, payload string) error {
	if err := b.rdb.Publish(ctx, b.channel(resource), string(t)+"\t"+payload).Err(); err != nil {
		return fmt.Errorf("locks/redis: publish: %w", err)
	}
	return nil
}

// Subscribe implements Backend. The cancel function must be called to release
// the underlying Redis subscription goroutine.
func (b *RedisBackend) Subscribe(ctx context.Context, resource string) (<-chan Event, func(), error) {
	sub := b.rdb.Subscribe(ctx, b.channel(resource))
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, nil, fmt.Errorf("locks/redis: subscribe %q: %w", resource, err)
	}
	out := make(chan Event, 32)
	done := make(chan struct{})
	go func() {
		defer close(out)
		ch := sub.Channel()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				typ, payload, _ := strings.Cut(msg.Payload, "\t")
				select {
				case out <- Event{Resource: resource, Type: EventType(typ), Payload: payload}:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	cancel := func() {
		close(done)
		_ = sub.Close()
	}
	return out, cancel, nil
}

// IncrementCounter implements Backend via INCRBY. Missing keys
// are initialised to 0 by Redis automatically before applying
// delta. Counter keys live under "<keyPrefix>counter:<key>" so
// they never collide with lock state keys.
func (b *RedisBackend) IncrementCounter(ctx context.Context, key string, delta int64) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("locks/redis: counter key must be non-empty")
	}
	v, err := b.rdb.IncrBy(ctx, b.counterKey(key), delta).Result()
	if err != nil {
		return 0, fmt.Errorf("locks/redis: counter incr %s: %w", key, err)
	}
	return v, nil
}

func (b *RedisBackend) counterKey(key string) string {
	return b.keyPrefix + "counter:" + key
}

// Close implements Backend. Closes the wrapped Redis client.
func (b *RedisBackend) Close() error {
	if err := b.rdb.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
		return fmt.Errorf("locks/redis: close: %w", err)
	}
	return nil
}
