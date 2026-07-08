// Package acl persists per-mailbox ACL state in a yarilo-acl file inside the
// folder's mailbox directory: the ACL lives with the mail data and does NOT
// follow INDEX=. One file per mailbox; on-disk format is the same ACL line
// encoding parsed and written by pkg/mailbox.
//
// Cross-process correctness comes from pkg/locks via the same
// MailboxKey the fileindex backend uses for that folder. Read-
// modify-write goes through withLock so concurrent SETACL on the
// same mailbox cannot interleave; pure reads (GETACL/MYRIGHTS)
// take the same lock briefly to avoid catching a torn file.
//
// Layout — folder → file, rooted at the mail root and using the driver's
// folder sub-layout via mailbox.FolderSubpath, so the yarilo-acl file sits
// in the mailbox directory. For maildir:
//
//	INBOX           → <mailroot>/yarilo-acl
//	Sent            → <mailroot>/.Sent/yarilo-acl
//
// mailbox.FolderSubpath is the single source of truth for the folder→dir
// mapping, shared with the mailbox backends.
package acl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	// mailRoot is the mailbox data root (MailPath, else Home). The ACL file
	// lives in the mailbox directory, so it does NOT follow INDEX=.
	mailRoot string
	// driver selects the per-folder folder sub-layout (maildir/mdbox/sdbox)
	// via mailbox.FolderSubpath, shared with the mailbox backends.
	driver string
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

// New constructs a Store. The ACL file lives in the mailbox directory, so
// the root is mailPath when set, else home (the per-namespace root: personal
// = UserInfo.Home; shared/public = the namespace location). driver selects
// the folder sub-layout; locker may be nil for tests / single-process runs.
func New(home, mailPath, driver, username, owner string, locker locks.Locker) *Store {
	root := home
	if mailPath != "" {
		root = mailPath
	}
	return &Store{
		mailRoot: root,
		driver:   driver,
		username: username,
		owner:    owner,
		locker:   locker,
	}
}

// Path returns the on-disk yarilo-acl path for folder. Exposed so
// callers (admin CLI, integration tests) can locate the file without
// re-deriving the layout. folder == "" is the local per-namespace-root
// default ACL; for maildir it collides with INBOX and is disabled — see
// rootDefaultDisabled.
func (s *Store) Path(folder string) string {
	return filepath.Join(s.mailRoot, mailbox.FolderSubpath(s.driver, folder, folder), FileName)
}

// rootDefaultDisabled reports whether the local namespace-root default ACL
// (folder == "") is unavailable because its path collides with INBOX's — the
// maildir case, where INBOX is the maildir root. A global ACL (separate
// configured directory) is the intended source of defaults in that setup.
func (s *Store) rootDefaultDisabled() bool {
	return s.Path("") == s.Path("INBOX")
}

// mailboxesRoot is the mailbox-tree root: the mail root for maildir,
// <mailroot>/mailboxes for dbox drivers. The namespace-wide yarilo-acl-list
// lives here.
func (s *Store) mailboxesRoot() string {
	switch s.driver {
	case "mdbox", "sdbox", "dbox":
		return filepath.Join(s.mailRoot, "mailboxes")
	default:
		return s.mailRoot
	}
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
//
// After the per-mailbox file write succeeds, the yarilo-acl-list
// namespace-wide index is updated in the same call so LIST
// optimisations see the change without a separate rebuild.
func (s *Store) Set(folder string, acl mailbox.ACL) error {
	if folder == "" && s.rootDefaultDisabled() {
		return fmt.Errorf("userstate/acl: namespace-root default ACL unavailable (collides with INBOX); use a global ACL instead")
	}
	if err := s.withLock(folder, func() error {
		return s.writeAtomicLocked(folder, acl)
	}); err != nil {
		return err
	}
	return s.ListUpdate(folder, acl)
}

// EffectiveFor resolves the user's effective rights on folder,
// walking ancestors until an explicit yarilo-acl file is found
// (first-ancestor-with-explicit-ACL semantics).
//
//   - isOwner == true: returns FullRights immediately without I/O.
//   - else: read folder's ACL. If present, return its Effective.
//     If absent, strip the last segment off folder and retry. After
//     all segments are stripped, take one final pass at the
//     namespace-root ACL (folder = ""). If no ACL was ever found,
//     returns the empty rights set.
//
// sep is the namespace's hierarchy separator (typically '/' for
// personal namespaces; the caller passes h.spec.Separator). When
// sep is the zero byte the walk is disabled and only folder itself
// is consulted — useful for tests and namespaces that explicitly
// opt out of inheritance.
//
// Inheritance is "first hit wins": once an ancestor with an ACL
// file is found, that file's positive / negative balance determines
// the rights. ACLs from deeper ancestors (including the root) are
// not merged in. The alternative (full-chain merge) breaks the
// principle of locality for shared-mailbox admin who expects
// setting one ACL to fully override the inherited one.
func (s *Store) EffectiveFor(folder, user string, groups []string, isOwner bool, sep byte) (mailbox.Rights, error) {
	if isOwner {
		return mailbox.FullRights, nil
	}
	cur := folder
	rootTried := false
	for {
		// The local namespace-root default is disabled when it would collide
		// with INBOX (maildir): the file there is INBOX's own, not a default.
		if cur == "" && s.rootDefaultDisabled() {
			return "", nil
		}
		acl, err := s.Get(cur)
		if err != nil {
			return "", err
		}
		if acl != nil {
			return acl.Effective(user, groups, false), nil
		}
		if cur == "" {
			rootTried = true
		}
		if sep == 0 {
			return "", nil
		}
		idx := lastSepIndex(cur, sep)
		if idx < 0 {
			if rootTried {
				return "", nil
			}
			cur = ""
			continue
		}
		cur = cur[:idx]
	}
}

// lastSepIndex returns the byte index of the last occurrence of sep
// in s, or -1 when none is present. Stripped out into its own helper
// so the caller can swap separators without restating the bytes import
// surface.
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
//
// Also drops every yarilo-acl-list entry for this mailbox so the
// namespace-wide index stays consistent.
func (s *Store) Remove(folder string) error {
	if err := s.withLock(folder, func() error {
		path := s.Path(folder)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("userstate/acl: remove %s: %w", path, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return s.ListRemove(folder)
}

// Rename moves the per-mailbox yarilo-acl file from oldFolder's
// index dir to newFolder's, and rewrites every yarilo-acl-list
// entry pointing at oldFolder to point at newFolder. Called from
// the IMAP RENAME handler so the index stays consistent across the
// structural change. Missing source file is a no-op (mailbox had
// no explicit ACL); the index rewrite still runs in case the
// caller previously seeded entries out-of-band.
//
// Both per-folder locks are taken in lexicographic order to mirror
// fileindex.withTwoFolderLocks so two RENAMEs cannot deadlock
// against each other.
func (s *Store) Rename(oldFolder, newFolder string) error {
	first, second := oldFolder, newFolder
	if first > second {
		first, second = second, first
	}
	err := s.withLock(first, func() error {
		return s.withLock(second, func() error {
			oldPath := s.Path(oldFolder)
			info, err := os.Stat(oldPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("userstate/acl: rename stat %s: %w", oldPath, err)
			}
			if info.IsDir() {
				return fmt.Errorf("userstate/acl: rename %s: source is a directory", oldPath)
			}
			newPath := s.Path(newFolder)
			if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
				return fmt.Errorf("userstate/acl: rename mkdir %s: %w", filepath.Dir(newPath), err)
			}
			if err := os.Rename(oldPath, newPath); err != nil {
				return fmt.Errorf("userstate/acl: rename %s → %s: %w", oldPath, newPath, err)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	return s.ListRename(oldFolder, newFolder)
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
	dst := s.Path(folder)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("userstate/acl: mkdir %s: %w", dir, err)
	}
	tmp := dst + ".tmp"
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
