// Package acl persists per-mailbox ACL state in a yarilo-acl file
// inside the folder's index directory (the same dir holding
// yarilo.index*). One file per mailbox; on-disk format matches
// Dovecot's dovecot-acl byte-for-byte so the same parser/encoder
// in pkg/mailbox serves both reads and writes.
//
// Cross-process correctness comes from pkg/locks via the same
// MailboxKey the fileindex backend uses for that folder. Read-
// modify-write goes through withLock so concurrent SETACL on the
// same mailbox cannot interleave; pure reads (GETACL/MYRIGHTS)
// take the same lock briefly to avoid catching a torn file.
//
// Layout — folder → file (Maildir-style, mirrors fileindex):
//
//	INBOX           → <home>/INBOX/yarilo-acl
//	Sent            → <home>/.Sent/yarilo-acl
//	Lists/announce  → <home>/.Lists/announce/yarilo-acl
//
// IndexDirFor in internal/storage/index/file is the single source
// of truth for the folder→dir mapping; this package depends on it.
package acl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// FileName is the on-disk per-folder ACL filename.
const FileName = "yarilo-acl"

// Store is a per-user, per-namespace ACL handle. Construct one per
// namespace handle at session open time; methods take the folder
// name relative to the namespace root (same convention as
// mailbox.UserMailbox).
type Store struct {
	// home is the namespace root used to derive per-folder dir paths
	// via file.IndexDirFor.
	home string
	// username is whose lock-key is acquired on every write. For
	// shared/public namespaces this is the accessing user — the
	// MailboxKey is per-(user, folder), so concurrent writes from
	// two different users to the same shared mailbox serialise
	// only when they collide on the same lock key (covered by
	// the fileindex's MailboxKey of the namespace owner; see
	// callers in internal/imap).
	username string
	owner    string
	locker   locks.Locker
}

// New constructs a Store rooted at home. Pass the per-namespace home
// (personal: UserInfo.Home; shared/public: synthetic UserInfo.Home
// set to the namespace location), the accessing username, the
// owner string for diagnostics, and the cluster locker (may be nil
// for tests / single-process dev runs).
func New(home, username, owner string, locker locks.Locker) *Store {
	return &Store{
		home:     home,
		username: username,
		owner:    owner,
		locker:   locker,
	}
}

// Path returns the on-disk yarilo-acl path for folder. Exposed so
// callers (admin CLI, integration tests) can locate the file without
// re-deriving the layout.
func (s *Store) Path(folder string) string {
	return filepath.Join(file.IndexDirFor(s.home, folder), FileName)
}

// Get returns the parsed ACL for folder. When the file does not
// exist, returns (nil, nil) — "no explicit ACL on this mailbox"
// is a normal state, not an error. Inheritance lookup (the
// first-ancestor-with-explicit-ACL walk) is the caller's job and
// lives in the evaluator that ships with PR E.
func (s *Store) Get(folder string) (mailbox.ACL, error) {
	var (
		out mailbox.ACL
		err error
	)
	werr := s.withLock(folder, func() error {
		out, err = s.loadLocked(folder)
		return err
	})
	if werr != nil {
		return nil, werr
	}
	return out, nil
}

// Set replaces the on-disk ACL with acl, encoded in canonical sorted
// order via ACL.Sorted(). Empty acl results in an empty file (zero
// bytes), not file removal — use Remove for that. Atomic via
// tmp+rename inside the folder's index dir.
func (s *Store) Set(folder string, acl mailbox.ACL) error {
	return s.withLock(folder, func() error {
		return s.writeAtomicLocked(folder, acl)
	})
}

// EffectiveFor resolves the user's effective rights on folder,
// walking ancestors until an explicit yarilo-acl file is found
// (Dovecot's first-ancestor-with-explicit-ACL semantics, see
// dovecot-2.4/src/plugins/acl/acl-backend-vfile.c:195-213).
//
//   - isOwner == true: returns FullRights immediately without I/O.
//   - else: read folder's ACL. If present, return its Effective.
//     If absent, strip the last segment off folder and retry,
//     repeating until the parent split yields nothing. If no ACL
//     was ever found, returns the empty rights set.
//
// sep is the namespace's hierarchy separator (typically '/' for
// personal namespaces; the caller passes h.spec.Separator). When
// sep is the zero byte the walk is disabled and only folder itself
// is consulted — useful for tests and namespaces that explicitly
// opt out of inheritance.
//
// Inheritance is "first hit wins": once a parent with an ACL file
// is found, that file's positive / negative balance determines the
// rights. ACLs from deeper ancestors are not merged in. This matches
// Dovecot; the alternative (full-chain merge) breaks the principle
// of locality for shared-mailbox admin who expects setting one ACL
// to fully override the inherited one.
func (s *Store) EffectiveFor(folder, user string, isOwner bool, sep byte) (mailbox.Rights, error) {
	if isOwner {
		return mailbox.FullRights, nil
	}
	cur := folder
	for {
		acl, err := s.Get(cur)
		if err != nil {
			return "", err
		}
		if acl != nil {
			return acl.Effective(user, false), nil
		}
		if sep == 0 {
			return "", nil
		}
		idx := lastSepIndex(cur, sep)
		if idx < 0 {
			return "", nil
		}
		cur = cur[:idx]
	}
}

// lastSepIndex returns the byte index of the last occurrence of sep
// in s, or -1 when none is present. Stripped out into its own helper
// so the caller can swap separators (Dovecot supports '/' and '.')
// without restating the bytes import surface.
func lastSepIndex(s string, sep byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return i
		}
	}
	return -1
}

// Remove deletes the yarilo-acl file. Idempotent — a missing file
// is not an error. Used by ACL admin paths that want to drop a
// mailbox back to "no explicit ACL" — after which EffectiveFor
// resumes walking ancestors for the inherited ACL.
func (s *Store) Remove(folder string) error {
	return s.withLock(folder, func() error {
		path := s.Path(folder)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("userstate/acl: remove %s: %w", path, err)
		}
		return nil
	})
}

// Update applies fn under the folder lock — fn receives the current
// ACL (nil when the file does not exist) and returns the new ACL
// to persist. Returning a nil ACL is treated as "leave on disk as-is";
// to drop entries, return an empty (non-nil) mailbox.ACL. Used by
// SETACL / DELETEACL which read-modify-write a single identifier.
func (s *Store) Update(folder string, fn func(mailbox.ACL) (mailbox.ACL, error)) error {
	return s.withLock(folder, func() error {
		current, err := s.loadLocked(folder)
		if err != nil {
			return err
		}
		next, err := fn(current)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		return s.writeAtomicLocked(folder, next)
	})
}

func (s *Store) loadLocked(folder string) (mailbox.ACL, error) {
	path := s.Path(folder)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("userstate/acl: open %s: %w", path, err)
	}
	defer f.Close()
	acl, err := mailbox.ParseACL(f)
	if err != nil {
		return nil, fmt.Errorf("userstate/acl: parse %s: %w", path, err)
	}
	return acl, nil
}

func (s *Store) writeAtomicLocked(folder string, acl mailbox.ACL) error {
	dir := file.IndexDirFor(s.home, folder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("userstate/acl: mkdir %s: %w", dir, err)
	}
	tmp := filepath.Join(dir, FileName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("userstate/acl: create tmp %s: %w", tmp, err)
	}
	body := acl.Sorted().String()
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: close %s: %w", tmp, err)
	}
	dst := filepath.Join(dir, FileName)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("userstate/acl: rename %s → %s: %w", tmp, dst, err)
	}
	return nil
}

func (s *Store) withLock(folder string, fn func() error) error {
	if s.locker == nil {
		return fn()
	}
	key := locks.MailboxKey(s.username, folder)
	if s.locker.HoldsResource(key) {
		// Outer caller already owns this MailboxKey (e.g. an admin
		// operation that takes the lock once and drives several ACL
		// edits). Skip re-acquire — locks are not reentrant on the
		// remote backend.
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, s.locker, key, s.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("userstate/acl: lock %s: %w", key, err)
	}
	defer func() { _ = s.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
