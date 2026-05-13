package lmtp

import "sync"

// userSemaphore enforces per-user delivery concurrency (lmtp_user_concurrency_limit).
// limit == 0 means no limiting. Each user gets a buffered channel of size limit;
// acquire sends to the channel (non-blocking), release receives from it.
type userSemaphore struct {
	mu    sync.Mutex
	sems  map[string]chan struct{}
	limit int
}

func newUserSemaphore(limit int) *userSemaphore {
	return &userSemaphore{sems: make(map[string]chan struct{}), limit: limit}
}

// acquire returns true and takes a slot for username, or returns false when the
// limit is already reached.
func (u *userSemaphore) acquire(username string) bool {
	u.mu.Lock()
	ch := u.sems[username]
	if ch == nil {
		ch = make(chan struct{}, u.limit)
		u.sems[username] = ch
	}
	u.mu.Unlock()

	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// release frees a slot for username.
func (u *userSemaphore) release(username string) {
	u.mu.Lock()
	ch := u.sems[username]
	u.mu.Unlock()
	if ch != nil {
		<-ch
	}
}
