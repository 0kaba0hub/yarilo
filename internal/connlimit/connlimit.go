// Package connlimit provides per-user@IP connection counting for IMAP/POP3/SMTP.
// It implements Dovecot's mail_max_userip_connections limit.
package connlimit

import "sync"

// Limiter tracks active connections per user@IP pair.
// A zero-value Limiter (or max ≤ 0) is unlimited.
type Limiter struct {
	mu    sync.Mutex
	count map[string]int
	max   int
}

// New creates a Limiter that allows at most max simultaneous connections
// per user@IP pair. max ≤ 0 means unlimited.
func New(max int) *Limiter {
	return &Limiter{count: make(map[string]int), max: max}
}

// Acquire increments the counter for (user, ip).
// Returns false (and does not increment) if the limit is already reached.
func (l *Limiter) Acquire(user, ip string) bool {
	if l.max <= 0 {
		return true
	}
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count[key] >= l.max {
		return false
	}
	l.count[key]++
	return true
}

// Release decrements the counter for (user, ip).
// Safe to call even if Acquire was never called.
func (l *Limiter) Release(user, ip string) {
	if l.max <= 0 {
		return
	}
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count[key] > 0 {
		l.count[key]--
		if l.count[key] == 0 {
			delete(l.count, key)
		}
	}
}

// Count returns the current connection count for (user, ip).
func (l *Limiter) Count(user, ip string) int {
	if l.max <= 0 {
		return 0
	}
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count[key]
}
