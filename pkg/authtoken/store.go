// Package authtoken implements the one-time session token store used by
// yarilo-auth to hand off authenticated sessions to backend pods without
// replaying credentials.
//
// Flow:
//  1. Login pod authenticates the client via the yarilo-auth AUTH command.
//  2. On success yarilo-auth issues a token (Store.Issue) and returns it in
//     the OK response as "token=<hex>".
//  3. Login pod forwards the token to the backend in the preamble
//     (XCLIENT TOKEN=<hex>).
//  4. Backend calls yarilo-auth VERIFY <id> <token>.
//  5. Store.Validate consumes the token (one-time) and returns the bound
//     username and session ID so the backend can enter authenticated state
//     without running passdb again.
package authtoken

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const defaultTTL = 60 * time.Second

type entry struct {
	username  string
	sessionID string
	service   string
	expiresAt time.Time
}

// Store issues and validates one-time session tokens. Each token is a
// 32-byte random value encoded as a 64-char hex string. It is valid for
// the configured TTL and consumed on the first successful Validate call.
//
// The background sweeper purges tokens that expire before they are
// consumed (e.g. backend died mid-handshake).
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	ttl     time.Duration
	done    chan struct{}
}

// New returns a ready-to-use Store. ttl ≤ 0 defaults to 60 s.
func New(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	s := &Store{
		entries: make(map[string]*entry),
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go s.sweep()
	return s
}

// Close stops the background sweeper.
func (s *Store) Close() {
	close(s.done)
}

// Issue generates a token bound to username, sessionID, and service. The caller
// must propagate the token to the backend before the TTL elapses.
func (s *Store) Issue(username, sessionID, service string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.entries[tok] = &entry{
		username:  username,
		sessionID: sessionID,
		service:   service,
		expiresAt: time.Now().Add(s.ttl),
	}
	s.mu.Unlock()
	return tok, nil
}

// Validate consumes tok and returns the associated username, sessionID, and service.
// Returns ("", "", "", false) when the token is unknown, already used, or expired.
func (s *Store) Validate(tok string) (username, sessionID, service string, ok bool) {
	s.mu.Lock()
	e, exists := s.entries[tok]
	if exists {
		delete(s.entries, tok)
	}
	s.mu.Unlock()
	if !exists || time.Now().After(e.expiresAt) {
		return "", "", "", false
	}
	return e.username, e.sessionID, e.service, true
}

func (s *Store) sweep() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for tok, e := range s.entries {
				if now.After(e.expiresAt) {
					delete(s.entries, tok)
				}
			}
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}
