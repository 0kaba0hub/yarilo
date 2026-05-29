package imap

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// specialUseStore persists per-user RFC 6154 special-use overrides set via
// CREATE (USE ...). Layout: one line per folder, "<folder>\t<attr>". The
// attr is a single MailboxAttr token like "\Sent" — RFC 6154 §3 forbids
// more than one special-use attr per folder.
//
// Lookups consult the on-disk overrides first; folders without an entry
// fall back to the SpecialUseDefaults map supplied at construction (driven
// by yarilo.yaml's protocol.imap.imap_special_use_defaults).
type specialUseStore struct {
	path     string
	username string
	owner    string
	locker   locks.Locker
	defaults map[string]imaplib.MailboxAttr // folder name → \Sent etc.
}

func newSpecialUseStore(home, username, owner string, locker locks.Locker, defaults map[string]string) *specialUseStore {
	mapped := make(map[string]imaplib.MailboxAttr, len(defaults))
	for folder, attr := range defaults {
		mapped[folder] = imaplib.MailboxAttr(attr)
	}
	return &specialUseStore{
		path:     filepath.Join(home, "special_use"),
		username: username,
		owner:    owner,
		locker:   locker,
		defaults: mapped,
	}
}

// load reads the on-disk overrides. Returns an empty map (not nil) when the
// file does not yet exist.
func (s *specialUseStore) load() (map[string]imaplib.MailboxAttr, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]imaplib.MailboxAttr), nil
		}
		return nil, fmt.Errorf("imap/special_use: open: %w", err)
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
		return nil, fmt.Errorf("imap/special_use: scan: %w", err)
	}
	return out, nil
}

// writeAtomic dumps overrides to disk via tmp+rename. Caller must hold the
// cross-process lock.
func (s *specialUseStore) writeAtomic(overrides map[string]imaplib.MailboxAttr) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("imap/special_use: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("imap/special_use: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	names := make([]string, 0, len(overrides))
	for folder := range overrides {
		names = append(names, folder)
	}
	sortStrings(names)
	for _, folder := range names {
		if _, err := fmt.Fprintf(bw, "%s\t%s\n", folder, overrides[folder]); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("imap/special_use: write: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/special_use: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/special_use: close: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("imap/special_use: rename: %w", err)
	}
	return nil
}

// withLock runs fn under the SubscriptionsKey lock — reusing it because
// metadata writes (subs + special-use) on the same user contend rarely and
// share a "metadata side-channel" semantic. nil locker = no cross-process
// guarantee, only tmp+rename atomicity.
func (s *specialUseStore) withLock(fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.SubscriptionsKey(s.username)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("imap/special_use: lock: %w", err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// Set records folder as having the supplied special-use attr. RFC 6154 §3:
// a folder carries at most one special-use; subsequent Set replaces.
// Idempotent if the same (folder, attr) pair is already on disk.
func (s *specialUseStore) Set(folder string, attr imaplib.MailboxAttr) error {
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

// Get returns the special-use attr for folder, considering on-disk
// overrides first and falling back to the configured defaults. Returns
// empty string when neither layer carries a match.
func (s *specialUseStore) Get(folder string) imaplib.MailboxAttr {
	overrides, err := s.snapshot()
	if err == nil {
		if attr, ok := overrides[folder]; ok {
			return attr
		}
	}
	return s.defaults[folder]
}

// snapshot loads the overrides under the lock. Used by LIST so the read is
// consistent against a concurrent CREATE (USE ...).
func (s *specialUseStore) snapshot() (map[string]imaplib.MailboxAttr, error) {
	var (
		overrides map[string]imaplib.MailboxAttr
		err       error
	)
	werr := s.withLock(func() error {
		overrides, err = s.load()
		return err
	})
	if werr != nil {
		return nil, werr
	}
	return overrides, nil
}
