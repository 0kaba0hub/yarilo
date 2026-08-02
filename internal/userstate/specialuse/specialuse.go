// Package specialuse persists per-user RFC 6154 special-use overrides
// set via IMAP CREATE (USE ...).
//
// Layout: one line per folder, "<folder>\t<attr>". The attr is a
// single MailboxAttr token like "\Sent" — RFC 6154 §3 forbids more
// than one special-use attr per folder.
//
// Lookups consult the on-disk overrides first; folders without an
// entry fall back to the SpecialUseDefaults map supplied at
// construction (driven by yarilo.yaml's
// protocol.imap.imap_special_use_defaults).
//
// Shared between the IMAP session path (internal/imap) and the
// backend-plane admin API (internal/backendapi).
package specialuse

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// Store is a per-user special-use override file.
type Store struct {
	path     string
	username string
	owner    string
	locker   locks.Locker
	defaults map[string]imaplib.MailboxAttr
}

// New constructs a Store rooted at home/special_use.
func New(home, username, owner string, locker locks.Locker, defaults map[string]string) *Store {
	mapped := make(map[string]imaplib.MailboxAttr, len(defaults))
	for folder, attr := range defaults {
		mapped[folder] = imaplib.MailboxAttr(attr)
	}
	return &Store{
		path:     filepath.Join(home, "special_use"),
		username: username,
		owner:    owner,
		locker:   locker,
		defaults: mapped,
	}
}

// Set records folder as having the supplied special-use attr.
// RFC 6154 §3: a folder carries at most one special-use; subsequent
// Set replaces. Idempotent if (folder, attr) is already on disk.
func (s *Store) Set(folder string, attr imaplib.MailboxAttr) error {
	return s.withLock(func() error {
		overrides, err := s.load()
		if err != nil {
			return err
		}
		if existing, ok := overrides[folder]; ok && existing == attr {
			return nil
		}
		overrides[folder] = attr
		return s.writeAtomic(overrides)
	})
}

// Delete drops the on-disk override for folder. The default mapping
// (if any) becomes visible again on the next Get. Idempotent.
func (s *Store) Delete(folder string) error {
	return s.withLock(func() error {
		overrides, err := s.load()
		if err != nil {
			return err
		}
		if _, ok := overrides[folder]; !ok {
			return nil
		}
		delete(overrides, folder)
		return s.writeAtomic(overrides)
	})
}

// Get returns the special-use attr for folder, considering on-disk
// overrides first and falling back to the configured defaults.
// Returns empty string when neither layer carries a match.
func (s *Store) Get(folder string) imaplib.MailboxAttr {
	overrides, err := s.Snapshot()
	if err == nil {
		if attr, ok := overrides[folder]; ok {
			return attr
		}
	}
	return s.defaults[folder]
}

// Snapshot loads the overrides under the lock. Used by LIST so the
// read is consistent against a concurrent CREATE (USE ...).
func (s *Store) Snapshot() (map[string]imaplib.MailboxAttr, error) {
	var (
		out map[string]imaplib.MailboxAttr
		err error
	)
	werr := s.withLock(func() error {
		out, err = s.load()
		return err
	})
	if werr != nil {
		return nil, werr
	}
	return out, nil
}

// Defaults returns a copy of the configured defaults map.
func (s *Store) Defaults() map[string]imaplib.MailboxAttr {
	out := make(map[string]imaplib.MailboxAttr, len(s.defaults))
	for k, v := range s.defaults {
		out[k] = v
	}
	return out
}

func (s *Store) load() (map[string]imaplib.MailboxAttr, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]imaplib.MailboxAttr), nil
		}
		return nil, fmt.Errorf("userstate/specialuse: open: %w", err)
	}
	defer f.Close()
	out := make(map[string]imaplib.MailboxAttr)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		folder, attr, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		out[folder] = imaplib.MailboxAttr(attr)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("userstate/specialuse: scan: %w", err)
	}
	return out, nil
}

func (s *Store) writeAtomic(overrides map[string]imaplib.MailboxAttr) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("userstate/specialuse: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("userstate/specialuse: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	names := make([]string, 0, len(overrides))
	for folder := range overrides {
		names = append(names, folder)
	}
	sort.Strings(names)
	for _, folder := range names {
		if _, err := fmt.Fprintf(bw, "%s\t%s\n", folder, overrides[folder]); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("userstate/specialuse: write: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/specialuse: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/specialuse: close: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/specialuse: rename: %w", err)
	}
	return nil
}

// withLock reuses locks.SubscriptionsKey — metadata writes (subs +
// special-use) on the same user contend rarely and share a "metadata
// side-channel" semantic. nil locker = no cross-process guarantee,
// only tmp+rename atomicity.
func (s *Store) withLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.SubscriptionsKey(s.username)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("userstate/specialuse: lock: %w", err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
