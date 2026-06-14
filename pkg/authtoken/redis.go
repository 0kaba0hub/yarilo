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
	keyPrefix = "yarilo:authtoken:"
	opTimeout = 5 * time.Second
)

type tokenPayload struct {
	Username  string `json:"u"`
	SessionID string `json:"s"`
	Service   string `json:"v"`
}

// RedisStore is a Redis-backed one-time token store.
// GETDEL provides atomic consume; Redis TTL replaces the background sweeper.
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedis returns a RedisStore. The caller owns the client lifecycle; Close is a no-op.
func NewRedis(client *redis.Client, ttl time.Duration) *RedisStore {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &RedisStore{client: client, ttl: ttl}
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
	if err := s.client.Set(ctx, keyPrefix+tok, payload, s.ttl).Err(); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *RedisStore) Validate(tok string) (username, sessionID, service string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	val, err := s.client.GetDel(ctx, keyPrefix+tok).Result()
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
