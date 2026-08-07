// Package acl persists per-mailbox ACL state in a yarilo-acl file inside the
// folder's mailbox directory: the ACL lives with the mail data and does NOT
// follow INDEX=. One file per mailbox; on-disk format is the ACL line encoding
// of pkg/mailbox.
//
// Cross-process writes serialise through pkg/locks under the same MailboxKey
// the fileindex backend uses; reads take the lock briefly to avoid a torn file.
//
// Layout uses mailbox.FolderSubpath (shared with the mailbox backends). For
// maildir:
//
//	INBOX           → <mailroot>/yarilo-acl
//	Sent            → <mailroot>/.Sent/yarilo-acl
package acl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FileName is the on-disk per-folder ACL filename.
const FileName = "yarilo-acl"

// Store is a per-user, per-namespace ACL handle. Methods take the folder name
// relative to the namespace root (same convention as mailbox.UserMailbox).
type Store struct {
	// mailRoot is the mailbox data root (MailPath, else Home); the ACL file
	// lives there, so it does NOT follow INDEX=.
	mailRoot string
	// driver selects the folder sub-layout via mailbox.FolderSubpath.
	driver string
	// separator is the IMAP hierarchy separator.
	separator string
	// escapeChar is the storage-name escape char; it must match the mailbox
	// driver's, or the ACL file lands beside a differently-named folder.
	escapeChar string
	// defaultsFromInbox resolves the namespace-root default from INBOX's ACL
	// instead of the (maildir-disabled) folder "" default. Private/shared only.
	defaultsFromInbox bool
	// globalsOnly ignores the per-mailbox files and evaluates only global.
	globalsOnly bool
	// global is the operator-configured global ACL merged into every
	// EffectiveFor with global precedence; nil when none.
	global *Global
	// username keys the lock acquired on every write; for shared/public
	// namespaces this is the accessing user.
	username string
	owner    string
	locker   locks.Locker

	// ttl bounds how long a cached parsed ACL is trusted before its file's
	// mtime+size are re-validated. Zero disables caching.
	ttl     time.Duration
	clock   func() time.Time
	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

// cacheEntry is one folder's cached ACL plus the file identity it was parsed
// from. at is the last time the entry was validated (read or stat-confirmed).
type cacheEntry struct {
	acl    mailbox.ACL
	exists bool
	mtime  int64
	size   int64
	at     time.Time
}

// New constructs a Store rooted at mailPath when set, else home. pol carries
// the operator ACL knobs; locker may be nil for tests / single-process runs.
func New(home, mailPath, driver, separator, escapeChar, username, owner string, pol Policy, locker locks.Locker) *Store {
	root := home
	if mailPath != "" {
		root = mailPath
	}
	s := &Store{
		mailRoot:          root,
		driver:            driver,
		separator:         mailbox.SepOrDefault(separator),
		escapeChar:        escapeChar,
		username:          username,
		owner:             owner,
		defaultsFromInbox: pol.DefaultsFromInbox,
		globalsOnly:       pol.GlobalsOnly,
		global:            pol.Global,
		locker:            locker,
		ttl:               pol.CacheTTL,
		clock:             time.Now,
	}
	if s.ttl > 0 {
		s.cache = make(map[string]cacheEntry)
	}
	return s
}

// RootFileName is the namespace-root ACL, kept in a file of its own.
//
// It cannot be yarilo-acl: on maildir INBOX *is* the mail root, so the root's
// ACL and INBOX's would be one file and each would be read as the other. That
// collision used to disable the root ACL entirely, which left a shared
// namespace with nowhere to grant the create right -- no user could make the
// first mailbox in it, and no admin call could change that (#1091).
//
// A distinct name removes the collision rather than working around it, and it
// is the same shape of fix as giving FTS its own root: two things that are not
// the same thing stop sharing a location.
const RootFileName = "yarilo-acl-root"

// Path returns the on-disk ACL path for folder. folder == "" is the
// namespace-root ACL, which lives in RootFileName beside the mailbox tree.
func (s *Store) Path(folder string) string {
	if folder == "" {
		return filepath.Join(s.mailboxesRoot(), RootFileName)
	}
	return filepath.Join(s.mailRoot, mailbox.FolderSubpathEscaped(s.driver, folder, folder, s.separator, s.escapeChar), FileName)
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

// Get returns the parsed ACL for folder, or (nil, nil) when the file does not
// exist ("no explicit ACL on this mailbox" is a normal state). Inheritance
// lookup is the caller's job.
func (s *Store) Get(folder string) (mailbox.ACL, error) {
	if s.ttl > 0 {
		return s.getCached(folder)
	}
	return s.getUncached(folder)
}

// getUncached reads and parses the ACL file under the folder lock.
func (s *Store) getUncached(folder string) (mailbox.ACL, error) {
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

// getCached serves ACLs from an in-process cache validated against the file's
// mtime+size. Within the TTL the parsed ACL is trusted with no I/O; past it a
// lock-free stat confirms the file is unchanged or triggers a reload. Safe
// without the remote lock because writes land via atomic tmp+rename.
func (s *Store) getCached(folder string) (mailbox.ACL, error) {
	now := s.clock()

	s.cacheMu.Lock()
	ent, ok := s.cache[folder]
	s.cacheMu.Unlock()
	if ok && now.Sub(ent.at) < s.ttl {
		return ent.acl, nil
	}

	fi, statErr := os.Stat(s.Path(folder))
	switch {
	case statErr != nil && os.IsNotExist(statErr):
		s.putCache(folder, cacheEntry{exists: false, at: now})
		return nil, nil
	case statErr != nil:
		return nil, fmt.Errorf("userstate/acl: stat %s: %w", s.Path(folder), statErr)
	}

	if ok && ent.exists && ent.mtime == fi.ModTime().UnixNano() && ent.size == fi.Size() {
		ent.at = now
		s.putCache(folder, ent)
		return ent.acl, nil
	}

	acl, err := s.getUncached(folder)
	if err != nil {
		return nil, err
	}
	s.putCache(folder, cacheEntry{
		acl:    acl,
		exists: true,
		mtime:  fi.ModTime().UnixNano(),
		size:   fi.Size(),
		at:     now,
	})
	return acl, nil
}

func (s *Store) putCache(folder string, ent cacheEntry) {
	s.cacheMu.Lock()
	s.cache[folder] = ent
	s.cacheMu.Unlock()
}

// invalidate drops a folder's cache entry after a local write. Cross-process
// writes are caught by the mtime+size re-validation.
func (s *Store) invalidate(folder string) {
	if s.cache == nil {
		return
	}
	s.cacheMu.Lock()
	delete(s.cache, folder)
	s.cacheMu.Unlock()
}

// Set replaces the on-disk ACL with acl in canonical sorted order. Empty acl
// writes an empty file (not removal — use Remove for that), atomic via
// tmp+rename. The yarilo-acl-list namespace-wide index is updated in the same
// call so LIST optimisations see the change.
// checkInsideRoot refuses a folder name whose ACL file would land outside the
// namespace this store serves.
//
// The store writes the files the IMAP commands read, so a name IMAP refuses
// must not be writable through the admin path -- and "." and "/" were. On
// maildir both resolve to <mailroot>/../yarilo-acl: outside the namespace,
// in the directory every user's tree shares, where another namespace's store
// reads it as its own root default. One admin call could hand rights to
// mailboxes in a namespace it never named (#1091).
//
// The question is asked of the resolved path rather than of the name, the same
// way the drivers ask it before a destructive operation (#1069). That also
// means the namespace-root ACL file, once it has one of its own, is permitted
// without this needing to learn its spelling: it is inside the root, which is
// the whole test.
func (s *Store) checkInsideRoot(folder string) error {
	return mailbox.GuardDestructivePath(s.mailboxesRoot(), s.Path(folder))
}

// errACLFileCorrupt marks a load failure as an unparseable file (#1140 p.5: one
// bad line fails the whole file) rather than an I/O error, so a write can choose
// to repair it -- see Update.
var errACLFileCorrupt = errors.New("acl file is corrupt")

// Set replaces a folder's ACL. It is Update with a constant function, so file
// and index are written in one place (see Update for the nil / removal
// semantics). The constant ignores the current value, so Set repairs a folder
// whose file is corrupt -- Update does not require the load to succeed for a
// function that produces a concrete result.
func (s *Store) Set(folder string, acl mailbox.ACL) error {
	return s.Update(folder, func(mailbox.ACL) (mailbox.ACL, error) { return acl, nil })
}

// EffectiveFor resolves the user's effective rights on folder, walking
// ancestors until an explicit yarilo-acl file is found (first-hit wins). An
// owner gets FullRights immediately without I/O; otherwise folder's ACL is
// read, and on absence the last path segment is stripped and retried, ending
// with one pass at the namespace-root ACL (folder = ""). No ACL found returns
// the empty rights set.
//
// sep == 0 disables the walk (only folder itself is consulted). Inheritance
// never merges deeper ancestors: the first ACL found fully determines the
// rights, so a shared-mailbox admin's single ACL overrides the inherited one.
func (s *Store) EffectiveFor(folder, user string, groups []string, isOwner bool, sep byte) (mailbox.Rights, error) {
	// Owner short-circuit before any read: the strong grant (§7.6) means no
	// entry can change it, so there is nothing to walk.
	if isOwner {
		return mailbox.FullRights, nil
	}
	var localACL mailbox.ACL
	if !s.globalsOnly {
		a, err := s.localACLFor(folder, sep)
		if err != nil {
			return "", err
		}
		localACL = a
	}
	globalACL := s.global.For(folder)
	// A non-owner with no ACL at all has no rights (the owner already returned
	// above).
	if localACL == nil && globalACL == nil {
		return "", nil
	}
	return mailbox.EffectiveWithGlobal(localACL, globalACL, user, groups, false), nil
}

// localACLFor resolves the local per-mailbox ACL that applies to folder: the
// first ancestor with an explicit ACL (first-hit wins), else the
// namespace-root default (INBOX with acl_defaults_from_inbox, else the local
// folder-"" default which is disabled for maildir). Returns nil when none.
// sep == 0 disables inheritance: only folder itself is consulted.
func (s *Store) localACLFor(folder string, sep byte) (mailbox.ACL, error) {
	for cur := folder; cur != ""; {
		acl, err := s.Get(cur)
		if err != nil {
			return nil, err
		}
		if acl != nil {
			return acl, nil
		}
		if sep == 0 {
			return nil, nil
		}
		idx := lastSepIndex(cur, sep)
		if idx < 0 {
			break
		}
		cur = cur[:idx]
	}
	return s.rootDefaultACL()
}

// rootDefaultACL loads the namespace-root default ACL: INBOX's ACL with
// acl_defaults_from_inbox, otherwise the root's own. Returns nil when neither
// exists.
//
// The root case used to be unavailable on maildir, where its file collided
// with INBOX's; it now has one of its own (RootFileName), so both sources are
// reachable on every driver (#1091).
func (s *Store) rootDefaultACL() (mailbox.ACL, error) {
	defFolder := ""
	if s.defaultsFromInbox {
		defFolder = "INBOX"
	}
	return s.Get(defFolder)
}

// lastSepIndex returns the byte index of the last sep in s, or -1 when absent.
func lastSepIndex(s string, sep byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return i
		}
	}
	return -1
}

// Remove deletes the yarilo-acl file (idempotent) and drops every
// yarilo-acl-list entry for the mailbox. The mailbox falls back to "no explicit
// ACL", after which EffectiveFor resumes walking ancestors.
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
	s.invalidate(folder)
	return s.ListRemove(folder)
}

// Rename moves the per-mailbox yarilo-acl file from oldFolder to newFolder and
// rewrites every yarilo-acl-list entry pointing at oldFolder. A missing source
// file is a no-op; the index rewrite still runs. Both per-folder locks are
// taken in lexicographic order (mirroring fileindex.withTwoFolderLocks) so two
// RENAMEs cannot deadlock.
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
	s.invalidate(oldFolder)
	s.invalidate(newFolder)
	return s.ListRename(oldFolder, newFolder)
}

// Update read-modify-writes the ACL under the folder lock: fn receives the
// current ACL (nil when absent) and returns the ACL to persist. A nil return
// leaves disk as-is; return an empty (non-nil) ACL to drop all entries.
// Update is the one write path for a folder's ACL: read-modify-write the file
// and the yarilo-acl-list index together, under the folder lock, so no reader
// sees the two disagree and no caller can update one and forget the other
// (#1147). fn returning nil means "no change" -- the file and index are both
// left alone; to clear a folder return an empty (non-nil) ACL, which writes an
// empty file and drops the folder's index rows.
func (s *Store) Update(folder string, fn func(mailbox.ACL) (mailbox.ACL, error)) error {
	if err := s.checkInsideRoot(folder); err != nil {
		return fmt.Errorf("userstate/acl: refusing folder %q: %w", folder, err)
	}
	err := s.withLock(folder, func() error {
		current, err := s.loadLocked(folder)
		var corruptErr error
		if err != nil {
			if !errors.Is(err, errACLFileCorrupt) {
				return err // an I/O error, not corruption: do not clobber the file
			}
			// A corrupt file is unreadable to the evaluator too, so the mailbox
			// is already inaccessible. Offer it to fn as empty: a function that
			// produces a concrete result (Set, SETACL) then repairs it. Remember
			// the error for the case where fn declines to.
			corruptErr, current = err, nil
		}
		next, err := fn(current)
		if err != nil {
			return err
		}
		if next == nil {
			// No change. If the file was corrupt and fn did not replace it, the
			// corruption stands -- surface it rather than a silent OK.
			return corruptErr
		}
		if err := s.writeAtomicLocked(folder, next); err != nil {
			return err
		}
		// Same lock as the file: the yarilo-acl-list index (a separate lock of
		// its own) is updated before this critical section ends, so no reader
		// sees the file changed and the index not.
		return s.ListUpdate(folder, next)
	})
	// Invalidate whenever the file may have been written -- including when
	// ListUpdate failed after the file write, so the cache does not keep serving
	// the pre-write rights on the call that reported the error. A no-op reload is
	// harmless.
	s.invalidate(folder)
	return err
}

func (s *Store) loadLocked(folder string) (mailbox.ACL, error) {
	path := s.Path(folder)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if legacy := s.legacyRootPath(folder); legacy != "" {
				return s.loadFrom(legacy)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("userstate/acl: open %s: %w", path, err)
	}
	defer f.Close()
	acl, err := mailbox.ParseACL(f)
	if err != nil {
		return nil, fmt.Errorf("userstate/acl: parse %s: %w: %w", path, errACLFileCorrupt, err)
	}
	return acl, nil
}

func (s *Store) writeAtomicLocked(folder string, acl mailbox.ACL) error {
	// Collapsed on the way out as well as on the way in: reading a file
	// normalises it, but a caller that assembled duplicates in memory would
	// otherwise write them and have them read back merged -- the file and what
	// the caller believes it wrote would differ (#1114).
	acl = acl.Collapse()
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
		// Outer caller already holds this MailboxKey; skip re-acquire, the
		// remote lock is not reentrant.
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

// legacyRootPath is where a namespace-root ACL written before RootFileName
// existed would be, or "" when there is nothing to fall back to.
//
// The root used to share the per-folder file name, so on the dbox layouts it
// lived at mailboxes/dbox-Mails/yarilo-acl -- a real file, with real grants, in
// any deployment that used a shared namespace before this change. Reading the
// new name and finding nothing would drop those grants silently, and the
// symptom (everyone in the shared namespace loses access after an upgrade)
// points nowhere near the cause.
//
// Not applied on maildir: there the old path is INBOX's own file, and reading
// it as the root default is exactly the confusion RootFileName removes. The
// equality check is what keeps the two apart, so this cannot resurrect it.
func (s *Store) legacyRootPath(folder string) string {
	if folder != "" {
		return ""
	}
	legacy := filepath.Join(s.mailRoot,
		mailbox.FolderSubpathEscaped(s.driver, "", "", s.separator, s.escapeChar), FileName)
	if legacy == s.Path("INBOX") {
		return ""
	}
	return legacy
}

// loadFrom parses one ACL file, treating absence as no ACL.
func (s *Store) loadFrom(path string) (mailbox.ACL, error) {
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
		return nil, fmt.Errorf("userstate/acl: parse %s: %w: %w", path, errACLFileCorrupt, err)
	}
	return acl, nil
}
