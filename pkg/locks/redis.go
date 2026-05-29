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

// lockValue is the stored value: "<resource>|<owner>". Used to enforce
// owner-checked release/renew (an owner cannot release another owner's lock
// that happened to take the same ID — even though IDs are random 16-byte,
// this defends against ID forgery in a multi-tenant deployment). The reverse
// split is performed server-side in the Lua scripts (string.find on '|').
func lockValue(resource, owner string) string { return resource + "|" + owner }

// resKey for the secondary index "resource → lockID" — ensures one lock per
// resource at a time. Stored as a sibling key with the same TTL.
func (b *RedisBackend) resKey(resource string) string {
	return b.keyPrefix + "res:" + resource
}

func (b *RedisBackend) lockKey(lockID string) string {
	return b.keyPrefix + "id:" + lockID
}

func (b *RedisBackend) channel(resource string) string {
	return b.chPrefix + resource
}

// acquireScript: take both the resource-index key and the lock-id key
// atomically, or return the current owner if the resource is held.
//
//	KEYS[1] = resource index key   (yarilo:locks:res:<resource>)
//	KEYS[2] = lock id key          (yarilo:locks:id:<lockID>)
//	ARGV[1] = lockID
//	ARGV[2] = lockValue (resource|owner)
//	ARGV[3] = ttl_ms
//
//	returns {"OK"} on success, or {"BUSY", <current_owner>} on contention.
var acquireScript = redis.NewScript(`
local existing = redis.call("GET", KEYS[1])
if existing then
  local v = redis.call("GET", KEYS[1] .. ":val")
  local owner = ""
  if v then
    local sep = string.find(v, "|", 1, true)
    if sep then owner = string.sub(v, sep + 1) end
  end
  return {"BUSY", owner}
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[3])
redis.call("SET", KEYS[1] .. ":val", ARGV[2], "PX", ARGV[3])
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
return {"OK"}
`)

// releaseScript deletes both keys atomically by lock ID, validating that the
// resource-index still points at the same ID.
//
//	KEYS[1] = lock id key
//	ARGV[1] = lockID
//	ARGV[2] = key prefix (to build resource index key from lockValue)
//
//	returns 1 on success, 0 if not found.
var releaseScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
local sep = string.find(v, "|", 1, true)
if not sep then return 0 end
local resource = string.sub(v, 1, sep - 1)
local resKey = ARGV[2] .. "res:" .. resource
local current = redis.call("GET", resKey)
if current == ARGV[1] then
  redis.call("DEL", resKey)
  redis.call("DEL", resKey .. ":val")
end
redis.call("DEL", KEYS[1])
return 1
`)

// renewScript extends the TTL on both keys atomically.
//
//	KEYS[1] = lock id key
//	ARGV[1] = lockID
//	ARGV[2] = ttl_ms
//	ARGV[3] = key prefix
//
//	returns 1 on success, 0 if expired.
var renewScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
local sep = string.find(v, "|", 1, true)
if not sep then return 0 end
local resource = string.sub(v, 1, sep - 1)
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
		[]string{b.resKey(resource), b.lockKey(id)},
		id, lockValue(resource, owner), ttl.Milliseconds(),
	).Result()
	if err != nil {
		return "", "", fmt.Errorf("locks/redis: acquire: %w", err)
	}
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

// Close implements Backend. Closes the wrapped Redis client.
func (b *RedisBackend) Close() error {
	if err := b.rdb.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
		return fmt.Errorf("locks/redis: close: %w", err)
	}
	return nil
}
