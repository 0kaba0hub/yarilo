package imap

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// subscriptionStore persists the set of mailbox names the user has
// SUBSCRIBE'd. One folder name per line, lexicographically sorted on write
// so two processes writing the same set produce byte-identical files.
//
// Cross-process correctness comes from the SubscriptionsKey lock — every
// read-modify-write goes through the locker. When no Locker is wired (dev
// CLI), the file is still consistent because each write uses tmp+rename
// (atomic at the OS layer).
type subscriptionStore struct {
	path     string // <home>/subscriptions
	username string // for the lock key
	owner    string // <process>/<pid>/<user>
	locker   locks.Locker
}

func newSubscriptionStore(home, username, owner string, locker locks.Locker) *subscriptionStore {
	return newSubscriptionStoreFile(home, "subscriptions", username, owner, locker)
}

// newSubscriptionStoreFile is the explicit-filename variant introduced
// for NS-1b multi-namespace support: each namespace gets its own
// subscription file in the user's home (subscriptions for personal —
// preserved verbatim for pre-v1.21 upgrades — and subscriptions-<ns>
// for shared / public). Personal-only deployments continue to call
// newSubscriptionStore and read/write the original file.
func newSubscriptionStoreFile(home, filename, username, owner string, locker locks.Locker) *subscriptionStore {
	return &subscriptionStore{
		path:     filepath.Join(home, filename),
		username: username,
		owner:    owner,
		locker:   locker,
	}
}

// load reads the current subscription set. Returns an empty map (not nil)
// when the file does not yet exist — Subscribe gets to populate it.
func (s *subscriptionStore) load() (map[string]struct{}, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]struct{}), nil
		}
		return nil, fmt.Errorf("imap/subs: open: %w", err)
	}
	defer f.Close()
	subs := make(map[string]struct{})
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" {
			continue
		}
		subs[name] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("imap/subs: scan: %w", err)
	}
	return subs, nil
}

// writeAtomic dumps the supplied set to disk via tmp+rename. The caller
// must hold any cross-process lock; this only does the I/O.
func (s *subscriptionStore) writeAtomic(subs map[string]struct{}) error {
	names := make([]string, 0, len(subs))
	for name := range subs {
		names = append(names, name)
	}
	// Stable sort so concurrent writers of the same logical set produce
	// the same bytes — simplifies diffing across replicas.
	sortStrings(names)
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("imap/subs: mkdir: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("imap/subs: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	for _, name := range names {
		if _, err := fmt.Fprintln(bw, name); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("imap/subs: write: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/subs: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/subs: close: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/subs: rename: %w", err)
	}
	return nil
}

// withLock runs fn under the cross-process subs lock. When the Locker is
// nil (dev) fn runs unguarded — tmp+rename keeps the file readable, but
// concurrent writers can clobber each other's add/remove.
func (s *subscriptionStore) withLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.SubscriptionsKey(s.username)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("imap/subs: lock: %w", err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// Add records folder as subscribed. Idempotent.
func (s *subscriptionStore) Add(folder string) error {
	return s.withLock(func() error {
		subs, err := s.load()
		if err != nil {
			return err
		}
		if _, ok := subs[folder]; ok {
			return nil
		}
		subs[folder] = struct{}{}
		return s.writeAtomic(subs)
	})
}

// Remove drops folder from the subscription set. Idempotent.
func (s *subscriptionStore) Remove(folder string) error {
	return s.withLock(func() error {
		subs, err := s.load()
		if err != nil {
			return err
		}
		if _, ok := subs[folder]; !ok {
			return nil
		}
		delete(subs, folder)
		return s.writeAtomic(subs)
	})
}

// Snapshot returns the current set without holding the lock past the read.
// Callers MUST NOT mutate the returned map — it is shared with their
// internal accounting.
func (s *subscriptionStore) Snapshot() (map[string]struct{}, error) {
	var (
		subs map[string]struct{}
		err  error
	)
	werr := s.withLock(func() error {
		subs, err = s.load()
		return err
	})
	if werr != nil {
		return nil, werr
	}
	return subs, nil
}

// sortStrings keeps the package import surface minimal — avoids pulling
// the full "sort" package just for one string-slice sort.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j-1] > ss[j]; j-- {
			ss[j-1], ss[j] = ss[j], ss[j-1]
		}
	}
}
