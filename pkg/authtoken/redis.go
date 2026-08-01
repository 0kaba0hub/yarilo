package authtoken

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// defaultKeyPrefix namespaces token keys in Redis. It is the installation
	// boundary: two installations sharing one Redis must use distinct prefixes
	// or they collide on token keys (#939). Overridable via WithKeyPrefix.
	defaultKeyPrefix = "yarilo:authtoken:"
	opTimeout        = 5 * time.Second
)

// RedisOption tunes a RedisStore at construction, mirroring locks' options.
type RedisOption func(*RedisStore)

// WithKeyPrefix overrides the Redis key prefix. Empty is ignored (keeps the
// default), so a blank config value does not erase the namespace.
func WithKeyPrefix(p string) RedisOption {
	return func(s *RedisStore) {
		if p != "" {
			s.keyPrefix = p
		}
	}
}

type tokenPayload struct {
	Username  string `json:"u"`
	SessionID string `json:"s"`
	Service   string `json:"v"`
}

// RedisStore is a Redis-backed one-time token store.
// GETDEL provides atomic consume; Redis TTL replaces the background sweeper.
type RedisStore struct {
	client    *redis.Client
	ttl       time.Duration
	keyPrefix string
}

// NewRedis returns a RedisStore. The caller owns the client lifecycle; Close is a no-op.
func NewRedis(client *redis.Client, ttl time.Duration, opts ...RedisOption) *RedisStore {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	s := &RedisStore{client: client, ttl: ttl, keyPrefix: defaultKeyPrefix}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *RedisStore) Issue(username, sessionID, service string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	payload, err := json.Marshal(tokenPayload{Username: username, SessionID: sessionID, Service: service})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := s.client.Set(ctx, s.keyPrefix+tok, payload, s.ttl).Err(); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *RedisStore) Validate(tok string) (username, sessionID, service string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	val, err := s.client.GetDel(ctx, s.keyPrefix+tok).Result()
	if err != nil {
		return "", "", "", false
	}
	var p tokenPayload
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return "", "", "", false
	}
	return p.Username, p.SessionID, p.Service, true
}

// Close is a no-op; the caller manages the Redis client lifecycle.
func (s *RedisStore) Close() {}
