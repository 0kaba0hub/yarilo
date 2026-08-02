// Package subs persists the set of mailbox names a user has SUBSCRIBE'd.
//
// One folder name per line, lexicographically sorted on write so two
// processes writing the same set produce byte-identical files.
//
// Cross-process correctness comes from locks.SubscriptionsKey — every
// read-modify-write goes through the locker. When no Locker is wired
// (dev CLI), the file is still consistent because each write uses
// tmp+rename (atomic at the OS layer).
//
// Shared between the IMAP session path (internal/imap) and the
// backend-plane admin API (internal/backendapi) so on-disk format and
// locking stay identical.
package subs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// Store is a per-user subscription file. Construct one per (home,
// filename) pair — NS-1b assigns each namespace its own subscription
// file: personal keeps the pre-v1.21 "subscriptions" filename so
// upgrades preserve existing state, shared/public use
// "subscriptions-<ns>" siblings in the same home.
type Store struct {
	path     string
	username string
	owner    string
	locker   locks.Locker
}

// New constructs a Store rooted at home/<filename>.
func New(home, filename, username, owner string, locker locks.Locker) *Store {
	return &Store{
		path:     filepath.Join(home, filename),
		username: username,
		owner:    owner,
		locker:   locker,
	}
}

// Add records folder as subscribed. Idempotent.
func (s *Store) Add(folder string) error {
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
func (s *Store) Remove(folder string) error {
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

// Snapshot returns the current set. No distributed lock is acquired:
// writeAtomic uses tmp+rename so any read sees either the complete old
// or the complete new file — never a torn write.
// Callers MUST NOT mutate the returned map.
func (s *Store) Snapshot() (map[string]struct{}, error) {
	return s.load()
}

func (s *Store) load() (map[string]struct{}, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]struct{}), nil
		}
		return nil, fmt.Errorf("userstate/subs: open: %w", err)
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
		return nil, fmt.Errorf("userstate/subs: scan: %w", err)
	}
	return subs, nil
}

func (s *Store) writeAtomic(subs map[string]struct{}) error {
	names := make([]string, 0, len(subs))
	for name := range subs {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("userstate/subs: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("userstate/subs: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	for _, name := range names {
		if _, err := fmt.Fprintln(bw, name); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("userstate/subs: write: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/subs: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/subs: close: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/subs: rename: %w", err)
	}
	return nil
}

func (s *Store) withLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.SubscriptionsKey(s.username)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("userstate/subs: lock: %w", err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
