// Package maildir implements MailboxBackend for Maildir format.
// Filename: {secs}.M{usecs}P{pid}_{seq}.{hostname}:2,{flags}
// uidlist: yarilo-uidlist v3
package maildir

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Backend is the Maildir MailboxBackend factory. Holds only
// process-wide state (hostname, pid, counter); per-user state lives in userMailbox.
type Backend struct {
	hostname string
	pid      int
	counter  atomic.Uint64
	locker   locks.Locker
	writeSem chan struct{} // nil = unlimited
	listUTF8 bool          // true = UTF-8 on disk (default); false = modified-UTF-7
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend: every shared-file
// write takes a cross-process X lock on `mbox:<user>:<folder>`. A nil Locker
// keeps the in-process sync.Mutex only (single-process tests / dev).
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// WithMaxConcurrentWrites caps the number of concurrent Save() calls.
// Use 16-32 for spinning disks, 128-256 for SSDs. 0 means unlimited.
func WithMaxConcurrentWrites(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.writeSem = make(chan struct{}, n)
		}
	}
}

// WithListUTF8 sets the on-disk folder name encoding: true (default) UTF-8,
// false modified-UTF-7 (RFC 3501 §5.1.3) for legacy installations.
func WithListUTF8(v bool) Option { return func(b *Backend) { b.listUTF8 = v } }

// before storage and comparison. Default true.

// New creates a Maildir backend.
func New(opts ...Option) *Backend {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	b := &Backend{
		hostname: hostname,
		pid:      os.Getpid(),
		listUTF8: true,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u. Path resolution uses u.Home;
// usernames are never converted to paths.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	// No mail_path from userdb: default to <home>/Maildir, making INBOX the
	// maildir root rather than a home/INBOX subdirectory.
	mailPath := u.MailPath
	if mailPath == "" {
		mailPath = filepath.Join(u.Home, "Maildir")
	}
	explicit := true
	inboxPath := mailPath
	if u.InboxPath != "" {
		inboxPath = u.InboxPath
	}
	return &userMailbox{
		b:                b,
		home:             u.Home,
		mailPath:         mailPath,
		inboxPath:        inboxPath,
		explicitMailPath: explicit,
		controlDir:       u.ControlDir,
		separator:        mailbox.SepOrDefault(u.Separator),
		escapeChar:       u.StorageEscapeChar,
		username:         u.Username,
		owner:            locks.Owner(u.Username, u.SessionID),
		listUTF8:         b.listUTF8,
	}
}

// folderCache holds mtime-validated in-memory state for one maildir folder.
// folderCache is one folder's cached view of its uidlist and directory. Its own
// mutex, not the handle's: the reconcile's scan reaches it holding no mailbox
// lock since #1626, and guarding only the lookup in the map left every field
// inside it unguarded -- which the race detector finds as soon as a delivery
// moves the uidlist while a scan reads it.
type folderCache struct {
	mu       sync.Mutex
	uidMap   map[string]uint32
	guidMap  map[string][16]byte // explicit GUID overrides; empty for name-derived GUIDs
	uidMtime time.Time
	uidSize  int64
	entries  []os.DirEntry
	dirMtime time.Time
}

// snapshotUIDs returns the cached map when the uidlist has not moved. The map
// escapes the lock, so nothing may write into it afterwards -- see addUID.
func (c *folderCache) snapshotUIDs(mtime time.Time, size int64) (map[string]uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.uidMap != nil && mtime.Equal(c.uidMtime) && size == c.uidSize {
		return c.uidMap, true
	}
	return nil, false
}

func (c *folderCache) storeUIDs(m map[string]uint32, guids map[string][16]byte, mtime time.Time, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uidMap, c.guidMap, c.uidMtime, c.uidSize = m, guids, mtime, size
}

func (c *folderCache) guidOf(base string) ([16]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guidMap == nil {
		return [16]byte{}, false
	}
	g, ok := c.guidMap[base]
	return g, ok
}

// addUID records one message, replacing the maps rather than writing into them:
// snapshotUIDs hands its map out, and a scan holds it while it works, holding
// no folder lock since #1626. One copy per delivery leaves every reader with a
// map nobody can touch.
func (c *folderCache) addUID(base string, uid uint32, guid [16]byte, hasGUID bool, mtime time.Time, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uids := make(map[string]uint32, len(c.uidMap)+1)
	for k, v := range c.uidMap {
		uids[k] = v
	}
	uids[base] = uid
	c.uidMap = uids
	if hasGUID {
		guids := make(map[string][16]byte, len(c.guidMap)+1)
		for k, v := range c.guidMap {
			guids[k] = v
		}
		guids[base] = guid
		c.guidMap = guids
	}
	c.uidMtime, c.uidSize = mtime, size
}

// dirEntries returns the cached directory listing when the directory has not
// moved, and otherwise records the one the caller read.
func (c *folderCache) dirEntries(mtime time.Time) ([]os.DirEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries != nil && mtime.Equal(c.dirMtime) {
		return c.entries, true
	}
	return nil, false
}

func (c *folderCache) storeDirEntries(entries []os.DirEntry, mtime time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.dirMtime = entries, mtime
}

func (c *folderCache) invalidateDir() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// userMailbox is a per-session, per-user Maildir storage handle.
type userMailbox struct {
	b                *Backend
	home             string
	mailPath         string // effective mail storage root; equals home when MailPath is unset
	inboxPath        string // effective INBOX path; equals mailPath when InboxPath is unset
	explicitMailPath bool   // true when MailPath was set by userdb (changes INBOX layout)
	controlDir       string // CONTROL= override root (empty = co-located with home)
	separator        string // IMAP hierarchy separator; converted to "." on disk (maildir++)
	escapeChar       string // storage-name escape char; "" disables escaping
	username         string
	owner            string     // <process>/<pid>/<user> — passed to yarilo-locks for BUSY diagnostics
	listUTF8         bool       // mirrors Backend.listUTF8
	mu               sync.Mutex // in-process fast-path; cross-process barrier is b.locker
	cacheMu          sync.Mutex // guards cache; the scan reaches it holding no mailbox lock
	// inSection is non-zero while a reconcile's apply phase holds the folder
	// lock; the filesystem calls made there are counted (#1626).
	inSection  atomic.Int32
	sectionFS  atomic.Int32            // stats made inside the section
	sectionDir atomic.Int32            // directory reads made inside the section
	cache      map[string]*folderCache // keyed by folder name; lazy-initialised
}

// withMailboxLock runs fn under the in-process mutex, then the cross-process
// yarilo-locks X lock (only when b.locker is non-nil).
// withMailboxLockSite takes the folder's cross-process lock and records which
// call took it: this driver and the index take the same key, so a total without
// the caller says nothing about which one to change (#1630).
func (u *userMailbox) withMailboxLockSite(folder, site string, fn func() error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.b.locker == nil {
		return fn()
	}
	key := locks.MailboxKey(u.username, folder)
	// Re-entrancy: an outer scope already holds this resource for a batch
	// (POP3 QUIT / multi-message EXPUNGE); skip Acquire to avoid a same-owner
	// BUSY loop.
	if u.b.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("maildir/lock %s: %w", folder, err)
	}
	metricLockAcquired.WithLabelValues(site).Inc()
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

func (u *userMailbox) Init() error {
	if u.explicitMailPath {
		// Explicit mail_path: INBOX is the maildir root; cur/new/tmp go
		// directly under inboxPath.
		for _, sub := range []string{"cur", "new", "tmp"} {
			if err := os.MkdirAll(filepath.Join(u.inboxPath, sub), 0o700); err != nil {
				return fmt.Errorf("maildir/init: %w", err)
			}
		}
	} else {
		// Legacy layout: INBOX is a subdirectory of home.
		for _, sub := range []string{"INBOX/cur", "INBOX/new", "INBOX/tmp"} {
			if err := os.MkdirAll(filepath.Join(u.home, sub), 0o700); err != nil {
				return fmt.Errorf("maildir/init: %w", err)
			}
		}
	}
	return u.adoptFolderNames()
}

// adoptFolderNames brings the folder directories to this deployment's encoding.
// Where the two spellings differ nothing matches by name and a folder is listed
// as mojibake, selectable under no name a client can send (#1586, #1593).
//
// Level by level: splitting on the layout separator is safe because modified
// base64 is A-Z a-z 0-9 '+' ',', so '.' never appears inside an encoded run. On
// Init, one directory read when there is nothing to rename.
func (u *userMailbox) adoptFolderNames() error {
	// Maildir++ keeps every folder as a dotted directory beside INBOX, in the
	// mail path.
	root := u.mailPath
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("maildir/adopt names: read %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") || e.Name() == "." || e.Name() == ".." {
			continue
		}
		want, ok := u.adoptedDirName(e.Name())
		if !ok || want == e.Name() {
			continue
		}
		target := filepath.Join(root, want)
		if _, serr := os.Stat(target); serr == nil {
			// Two folders must never become one: that is a loss no later step
			// can undo, and a store in that shape needs a person.
			return fmt.Errorf("maildir/adopt names: %s would become %s, which exists", e.Name(), want)
		}
		if rerr := os.Rename(filepath.Join(root, e.Name()), target); rerr != nil {
			return fmt.Errorf("maildir/adopt names: rename %s: %w", e.Name(), rerr)
		}
		slog.Info("maildir: brought a folder name to this deployment's encoding",
			"user", u.username, "from", e.Name(), "to", want, "list_utf8", u.listUTF8)
	}
	return nil
}

// adoptedDirName returns the directory name this deployment would write for a
// maildir++ directory currently named name, and whether it could tell.
func (u *userMailbox) adoptedDirName(name string) (string, bool) {
	levels := strings.Split(strings.TrimPrefix(name, "."), ".")
	changed := false
	for i, level := range levels {
		if level == "" {
			continue
		}
		if isASCIIName(level) && !strings.Contains(level, "&") {
			// The two encodings agree on plain ASCII. The ampersand is the
			// exception: it is the escape in modified UTF-7, so a level
			// carrying one is not the same string in both.
			continue
		}
		var want string
		if u.listUTF8 {
			decoded, derr := mboxenc.FromModUTF7(level)
			if derr != nil {
				// Not their encoding, so nothing to bring across. It may be
				// exactly what a user typed.
				return name, false
			}
			want = decoded
		} else {
			// Already theirs if it survives a decode and re-encode unchanged;
			// encoding it again would escape its ampersand and produce the
			// double encoding this exists to remove.
			if decoded, derr := mboxenc.FromModUTF7(level); derr == nil && mboxenc.ToModUTF7(decoded) == level {
				continue
			}
			want = mboxenc.ToModUTF7(level)
		}
		if want != level {
			levels[i] = want
			changed = true
		}
	}
	if !changed {
		return name, true
	}
	return "." + strings.Join(levels, "."), true
}

func isASCIIName(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Create provisions the cur/new/tmp triplet for a folder under the X lock.
func (u *userMailbox) Create(folder string) error {
	if err := u.checkName(folder); err != nil {
		return err
	}
	return u.withMailboxLockSite(folder, lockSiteCreate, func() error {
		base := u.folderPath(folder)
		for _, sub := range []string{"cur", "new", "tmp"} {
			if err := os.MkdirAll(filepath.Join(base, sub), 0o700); err != nil {
				return fmt.Errorf("maildir/create: %w", err)
			}
		}
		return nil
	})
}

// Delete removes the entire folder tree (cur/new/tmp + uidlist + markers)
// under the X lock.
func (u *userMailbox) Delete(folder string) error {
	if err := u.checkName(folder); err != nil {
		return err
	}
	return u.withMailboxLockSite(folder, lockSiteDelete, func() error {
		path := u.folderPath(folder)
		// Last check before the removal, on the resolved path rather than the
		// name: whatever was validated above, this must not be the mail root.
		// On maildir++ INBOX *is* that root, so the difference between
		// removing a folder and removing an account is one path (#1069).
		if err := mailbox.GuardDestructivePath(u.mailPath, path, u.inboxPath); err != nil {
			return err
		}
		return os.RemoveAll(path)
	})
}

// Rename renames a folder on disk, holding the X lock on both names in
// lexicographic order so concurrent Renames cannot deadlock.
func (u *userMailbox) Rename(oldName, newName string) error {
	if err := u.checkName(oldName); err != nil {
		return err
	}
	if err := u.checkName(newName); err != nil {
		return err
	}
	return u.withTwoMailboxLocks(oldName, newName, lockSiteRename, func() error {
		from, to := u.folderPath(oldName), u.folderPath(newName)
		// Either end landing on the root is the same fault: renaming the root
		// away is as destructive as removing it, and renaming onto it buries
		// the mailbox under a folder.
		if err := mailbox.GuardDestructivePath(u.mailPath, from, u.inboxPath); err != nil {
			return err
		}
		if err := mailbox.GuardDestructivePath(u.mailPath, to, u.inboxPath); err != nil {
			return err
		}
		return os.Rename(from, to)
	})
}

// withTwoMailboxLocks takes both per-folder X locks in lexicographic order.
// Same ordering as the index side so a Rename rippling through both backends
// cannot deadlock.
func (u *userMailbox) withTwoMailboxLocks(folderA, folderB, site string, fn func() error) error {
	if u.b.locker == nil {
		return fn()
	}
	a, b := folderA, folderB
	if a > b {
		a, b = b, a
	}
	keyA := locks.MailboxKey(u.username, a)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if !u.b.locker.HoldsResource(keyA) {
		lkA, err := locks.Acquire(ctx, u.b.locker, keyA, u.owner, 30*time.Second)
		if err != nil {
			return fmt.Errorf("maildir/lock %s: %w", a, err)
		}
		metricLockAcquired.WithLabelValues(site).Inc()
		defer func() { _ = u.b.locker.Unlock(ctx, lkA.ID) }()
	}
	if a == b {
		return fn()
	}
	keyB := locks.MailboxKey(u.username, b)
	if !u.b.locker.HoldsResource(keyB) {
		lkB, err := locks.Acquire(ctx, u.b.locker, keyB, u.owner, 30*time.Second)
		if err != nil {
			return fmt.Errorf("maildir/lock %s: %w", b, err)
		}
		metricLockAcquired.WithLabelValues(site).Inc()
		defer func() { _ = u.b.locker.Unlock(ctx, lkB.ID) }()
	}
	return fn()
}

// driverName labels this driver in the timings shared with the others. This is
// the baseline the packed drivers are compared against: a write to tmp and a
// rename, with no steps of its own worth naming. The one part it does report
// is the wait for a write slot, which every driver reports -- leaving it out
// here would let the packed drivers subtract a queue the baseline still
// carries.
const driverName = "maildir"

// Save streams r into tmp/ then atomically renames into cur/. uid comes from
// UserIndex.AllocateUID; Maildir does not encode it in the filename, so the
// uid→filename mapping is appended inline to the yarilo-uidlist sidecar for
// later List() / Fetch() resolution.
func (u *userMailbox) Save(folder string, r io.Reader, uid uint32, _ int64, flags []string, guid [16]byte) (string, uint32, [16]byte, error) {
	whole := time.Now()
	defer func() { mailboxmetrics.ObserveSave(driverName, time.Since(whole)) }()

	var noGUID [16]byte
	if err := u.checkName(folder); err != nil {
		return "", 0, noGUID, err
	}
	if u.b.writeSem != nil {
		// Waiting for a slot is somebody else's write, not this one's work.
		// Timed on every driver, or the comparison tilts: one of them would
		// subtract the queue from its own cost and the others would not.
		sem := time.Now()
		u.b.writeSem <- struct{}{}
		mailboxmetrics.ObserveSavePart(driverName, "sem", time.Since(sem))
		defer func() { <-u.b.writeSem }()
	}
	folderPath := u.folderPath(folder)
	now := time.Now()
	seq := u.b.counter.Add(1)
	basename := fmt.Sprintf("%d.M%dP%d_%d.%s",
		now.Unix(), now.UnixMicro()%1_000_000, u.b.pid, seq, u.b.hostname)

	tmpPath := filepath.Join(folderPath, "tmp", basename)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, noGUID, fmt.Errorf("maildir: create tmp: %w", err)
	}
	sc := &sizeCounter{}
	if _, err := io.Copy(f, io.TeeReader(r, sc)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", 0, noGUID, fmt.Errorf("maildir: write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return "", 0, noGUID, err
	}

	// Keywords go into the name too, and into the folder's keyword file, so a
	// message delivered with one is described by the store rather than only by
	// our index (#1601). The lock is already held by the caller of Save.
	sys, kw := splitFlagsAndKeywords(flags)
	letters, kerr := u.keywordLetters(folder, kw)
	if kerr != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return "", 0, noGUID, kerr
	}
	flagStr := encodeFlags(sys) + letters
	// ,S=<phys>,W=<virt> before :2,<flags> so List() reports both sizes
	// without reading the body.
	finalName := fmt.Sprintf("%s,S=%d,W=%d:2,%s", basename, sc.phys, sc.phys+sc.lfNoCR, flagStr)

	// Fresh base name, so the derived GUID is unique. A caller-supplied GUID
	// (migration) is pinned with an explicit uidlist override instead.
	effGUID := guidFromBase(finalName)
	override := guid != noGUID && guid != effGUID
	if override {
		effGUID = guid
	}

	if err := u.withMailboxLockSite(folder, lockSiteSave, func() error {
		dstPath := filepath.Join(folderPath, "cur", finalName)
		if err := os.Rename(tmpPath, dstPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("maildir: rename to cur: %w", err)
		}
		u.folderCacheFor(folder).invalidateDir()
		if uid != 0 || override {
			if err := u.appendUIDListLocked(folder, uid, finalName, override, effGUID); err != nil {
				_ = os.Remove(dstPath)
				return fmt.Errorf("maildir: uidlist: %w", err)
			}
		}
		return nil
	}); err != nil {
		return "", 0, noGUID, err
	}
	return finalName, sc.phys + sc.lfNoCR, effGUID, nil
}

// Move renames the message into dstFolder keeping its base name, so the derived
// GUID and with it EMAILID is unchanged (RFC 8474); only the ":2," trailer and
// the folder change. A base-name collision falls back to a fresh name plus an
// explicit uidlist GUID override.
func (u *userMailbox) Move(srcFolder, dstFolder, filename string, guid [16]byte) (string, [16]byte, error) {
	var noGUID [16]byte
	if srcFolder == dstFolder {
		return filename, guidFromBase(filename), nil
	}
	newName := filename
	outGUID := guid
	if outGUID == noGUID {
		outGUID = guidFromBase(filename)
	}
	err := u.withTwoMailboxLocks(srcFolder, dstFolder, lockSiteMove, func() error {
		srcPath := filepath.Join(u.folderPath(srcFolder), "cur", filename)
		dstDir := filepath.Join(u.folderPath(dstFolder), "cur")
		dstPath := filepath.Join(dstDir, newName)
		override := outGUID != guidFromBase(newName)
		if _, err := os.Lstat(dstPath); err == nil {
			// Base name taken: mint a fresh one and pin the GUID explicitly.
			oldBase := maildirBase(filename)
			trailer := filename[len(oldBase):] // ":2,<flags>"
			sizeInfo := ""                     // ",S=<phys>,W=<virt>"
			if i := strings.IndexByte(oldBase, ','); i >= 0 {
				sizeInfo = oldBase[i:]
			}
			now := time.Now()
			seq := u.b.counter.Add(1)
			newName = fmt.Sprintf("%d.M%dP%d_%d.%s%s%s",
				now.Unix(), now.UnixMicro()%1_000_000, u.b.pid, seq, u.b.hostname, sizeInfo, trailer)
			dstPath = filepath.Join(dstDir, newName)
			override = true
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("maildir: move rename: %w", err)
		}
		u.folderCacheFor(srcFolder).invalidateDir()
		u.folderCacheFor(dstFolder).invalidateDir()
		if override {
			if err := u.appendUIDListLocked(dstFolder, 0, newName, true, outGUID); err != nil {
				return fmt.Errorf("maildir: move uidlist: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", noGUID, err
	}
	return newName, outGUID, nil
}

// appendUIDListLocked appends one entry to the yarilo-uidlist v3 sidecar and
// updates the in-memory cache. Caller MUST hold the mailbox X lock.
func (u *userMailbox) appendUIDListLocked(folder string, uid uint32, filename string, guidOverride bool, guid [16]byte) error {
	if err := u.migrateLegacyUIDList(folder); err != nil {
		return err
	}
	path := u.uidListPath(folder)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, _ := f.Stat()
	if info != nil && info.Size() == 0 {
		fmt.Fprintf(f, "3 V%d N%d G%s\n", uint32(time.Now().Unix()), uid+1, randomGUID())
	}
	// v3 record: "<uid> [G<guid>] :<base filename>". The name is cut at the info
	// separator, as they cut it: keyed by the whole name a record stops matching
	// its own message the moment a flag changes it. The GUID field appears only
	// when it must differ from the name-derived one.
	if guidOverride {
		_, err = fmt.Fprintf(f, "%d G%s :%s\n", uid, hex.EncodeToString(guid[:]), maildirBase(filename))
	} else {
		_, err = fmt.Fprintf(f, "%d :%s\n", uid, maildirBase(filename))
	}
	if err != nil {
		return err
	}

	// Update the cache inline so the next readUIDList skips the file.
	if fi, statErr := f.Stat(); statErr == nil {
		u.folderCacheFor(folder).addUID(maildirBase(filename), uid, guid, guidOverride, fi.ModTime(), fi.Size())
	}
	return nil
}

// sizeCounter records bytes written and the count of lone LFs (not preceded by
// CR, which would gain a CR under CRLF normalisation).
type sizeCounter struct {
	phys   uint32
	lfNoCR uint32
	prevCR bool
}

func (c *sizeCounter) Write(p []byte) (int, error) {
	for _, b := range p {
		c.phys++
		if b == '\n' && !c.prevCR {
			c.lfNoCR++
		}
		c.prevCR = b == '\r'
	}
	return len(p), nil
}

func (u *userMailbox) Fetch(folder, filename string, _ bool) (io.ReadCloser, error) {
	p := filepath.Join(u.folderPath(folder), "cur", filename)
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("maildir: fetch %s: %w", filename, err)
	}
	return f, nil
}

func (u *userMailbox) Remove(folder, filename string) error {
	p := filepath.Join(u.folderPath(folder), "cur", filename)
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	u.folderCacheFor(folder).invalidateDir()
	return nil
}

func (u *userMailbox) List(folder string) ([]*mailbox.MessageMeta, error) {
	dir := filepath.Join(u.folderPath(folder), "cur")

	dirFi, statErr := os.Stat(dir)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, nil
	}
	if statErr != nil {
		return nil, statErr
	}

	c := u.folderCacheFor(folder)
	entries, cached := c.dirEntries(dirFi.ModTime())
	if !cached {
		var err error
		entries, err = os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		c.storeDirEntries(entries, dirFi.ModTime())
	}

	uidMap, err := u.readUIDList(folder)
	if err != nil {
		return nil, err
	}
	kwNames := u.keywordNames(folder)

	var msgs []*mailbox.MessageMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		flags, keywords := decodeFlagsWith(name, kwNames)
		phys, virt, hasPhys, _ := parseSizeInfo(name)
		var sz uint32
		switch {
		case hasPhys:
			sz = phys
		default:
			if info, _ := e.Info(); info != nil {
				sz = uint32(info.Size())
			}
		}
		uid := uidMap[maildirBase(name)]
		msgs = append(msgs, &mailbox.MessageMeta{
			UID:      uid,
			Filename: name,
			Flags:    flags,
			Keywords: keywords,
			Size:     sz,
			VSize:    virt,
			GUID:     u.guidFor(folder, name),
		})
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].UID < msgs[j].UID
	})
	return msgs, nil
}

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	_, err := os.Stat(u.folderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (u *userMailbox) ListFolders() ([]mailbox.FolderEntry, error) {
	entries, err := os.ReadDir(u.mailPath)
	if err != nil {
		return nil, err
	}
	// maildir++ is flat: every ".<name>" dir is selectable and hierarchy lives
	// in the dotted name, so there are no \NoSelect containers.
	folders := []mailbox.FolderEntry{{Name: "INBOX", Selectable: true}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "INBOX" {
			continue
		}
		if !strings.HasPrefix(name, ".") {
			continue
		}
		disk := strings.TrimPrefix(name, ".")
		logical := disk
		if !u.listUTF8 {
			decoded, decErr := fromModUTF7(disk)
			if decErr != nil {
				continue // skip malformed names silently
			}
			logical = decoded
		}
		// maildir++ stores hierarchy flat with "."; map it back to the
		// namespace's IMAP separator (every "." is a level). With escaping on,
		// a "." the client wrote literally is not one of those levels: it is
		// an escape sequence, decoded per level after the split.
		if u.escapeChar != "" {
			parts := strings.Split(logical, ".")
			for i, p := range parts {
				parts[i] = mailbox.UnescapeStorageName(p, u.escapeChar)
			}
			logical = strings.Join(parts, u.separator)
		} else if u.separator != "." {
			logical = strings.ReplaceAll(logical, ".", u.separator)
		}
		folders = append(folders, mailbox.FolderEntry{Name: logical, Selectable: true})
	}
	return folders, nil
}

// Scan walks cur/ + new/ and returns one ScanRecord per message. Flags and
// size come from the filename (size from the "S=" infix, else os.Stat);
// InternalDate from the file mtime. GUID is left zero — Maildir filenames
// carry no stable GUID, so the rebuild flow must preserve the index's GUID
// for matched filenames.
func (u *userMailbox) Scan(folder string) ([]mailbox.ScanRecord, error) {
	if u.inSection.Load() > 0 {
		u.sectionDir.Add(1) // a walk under the lock is what #1626 took apart
	}
	// Warm the uidlist cache so explicit GUID overrides win over the derived
	// value; a missing uidlist just leaves every GUID name-derived.
	_, _ = u.readUIDList(folder)
	out := make([]mailbox.ScanRecord, 0, 128)
	kwNames := u.keywordNames(folder)
	for _, sub := range []string{"cur", "new"} {
		dir := filepath.Join(u.folderPath(folder), sub)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("maildir/scan: read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			flags, keywords := decodeFlagsWith(name, kwNames)
			phys, virt, hasPhys, _ := parseSizeInfo(name)
			info, statErr := e.Info()
			var sz uint32
			var mtime time.Time
			switch {
			case hasPhys:
				sz = phys
			case statErr == nil:
				sz = uint32(info.Size())
			}
			if statErr == nil {
				mtime = info.ModTime()
			}
			rec := mailbox.ScanRecord{
				Filename:     name,
				Size:         sz,
				VSize:        virt,
				InternalDate: mtime,
				Flags:        append([]string(nil), flags...),
				Keywords:     append([]string(nil), keywords...),
				GUID:         u.guidFor(folder, name),
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

func (u *userMailbox) Close() error { return nil }

// ProactiveScan reports that the on-disk state may change out of band (MDA
// delivery into new/, another MUA renaming for flags), so the index must be
// reconciled by scanning on SELECT. Index-authoritative drivers (dbox) omit
// this and self-heal reactively.
func (u *userMailbox) ProactiveScan() bool { return true }

// guidFor returns the message GUID for a stored file: the explicit uidlist
// override when one exists, else the name-derived value. Never zero.
func (u *userMailbox) guidFor(folder, filename string) [16]byte {
	if g, ok := u.folderCacheFor(folder).guidOf(maildirBase(filename)); ok {
		return g
	}
	return guidFromBase(filename)
}

// maildirBase returns everything before the ":" info separator — the stable
// identity of a maildir filename. A flag change renames only the ":2,<flags>"
// trailer, so names sharing a base are the same message and keep the same UID.
func maildirBase(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}

// guidFromBase derives the per-message GUID from the maildir base name. The
// base is unique per message and never rewritten: a flag change touches only
// the ":2," trailer and Move keeps it across folders, so EMAILID survives both
// and an index rebuild recomputes it.
func guidFromBase(filename string) [16]byte {
	sum := sha256.Sum256([]byte(maildirBase(filename)))
	var g [16]byte
	copy(g[:], sum[:16])
	return g
}

// moveNewToCurLocked moves every file from new/ into cur/, appending the ":2,"
// info marker for a message with no flags. The MDA delivers into new/; the rest
// of the driver (Fetch, Remove, List) only looks in cur/. Caller holds the lock.
func (u *userMailbox) moveNewToCurLocked(folder string) error {
	base := u.folderPath(folder)
	newDir := filepath.Join(base, "new")
	curDir := filepath.Join(base, "cur")
	entries, err := os.ReadDir(newDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("maildir/sync: read new: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		curName := name
		if !strings.ContainsRune(name, ':') {
			curName = name + ":2,"
		}
		if err := os.Rename(filepath.Join(newDir, name), filepath.Join(curDir, curName)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // moved by a concurrent sync
			}
			return fmt.Errorf("maildir/sync: move new->cur %s: %w", name, err)
		}
	}
	return nil
}

// ReconcileIndex brings idx into agreement with the physical maildir: new/ is
// migrated into cur/, messages match by base name so a flag rename keeps its
// UID, new files take a UID from the index's allocator, a vanished one is
// expunged with a tombstone. A message whose name has not changed is left
// alone -- the index is authoritative for flags this server set, which do not
// rename the file.
func (u *userMailbox) ReconcileIndex(idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, error) {
	var st mailbox.SyncStats
	adopted := false
	// The move precedes the scan because it renames, and it is asked about
	// before the lock because every acquisition is a round trip -- one taken to
	// find an empty directory is paid on every poll (#1630). A message landing
	// after the check waits for the next pass.
	movedNew := u.hasNewMail(folder.Name)
	if movePhaseProbe != nil {
		movePhaseProbe(movedNew)
	}
	if movedNew {
		if err := u.withMailboxLockSite(folder.Name, lockSiteReconcileMove, func() error {
			return u.moveNewToCurLocked(folder.Name)
		}); err != nil {
			return st, err
		}
	}

	// The walk, holding nothing. A flag change renames only the part after
	// ":2,", and everything here is keyed by the base name -- so the scan is
	// sound about which messages exist and unsound about the flags they carry.
	scanned, err := u.Scan(folder.Name)
	if err != nil {
		return st, fmt.Errorf("maildir/sync: scan: %w", err)
	}
	if afterScan != nil {
		afterScan()
	}

	// Nothing to apply, no lock at all: fifty sessions polling one folder each
	// took it to find the first had done the work (#1630). The comparison needs
	// no lock -- the index answers without one, and the section re-reads under
	// the lock before writing. A stale answer errs toward taking the lock: a
	// record it missed shows up as a file the index does not know.
	if u.reconcileIsClean(idx, folder, scanned) {
		return st, nil
	}

	err = u.withMailboxLockSite(folder.Name, lockSiteReconcileApply, func() error {
		u.inSection.Add(1)
		defer u.inSection.Add(-1)
		// A store being taken over: its uidlist already names a UID space.
		// Here rather than in its own acquisition -- it must precede the
		// appends, and nothing in the scan depends on it.
		if a, ok := idx.(mailbox.UIDSpaceAdopter); ok {
			if uidValidity, nextUID, have := u.UIDSpace(folder.Name); have {
				aerr := a.AdoptUIDSpace(folder.ID, uidValidity, nextUID)
				switch {
				case aerr == nil:
					adopted = true
				case errors.Is(aerr, mailbox.ErrUIDSpaceInUse):
					// An ordinary folder with mail in it, which is most of them.
					// Whether a folder is empty is the index's to answer, not a
					// caller's: the handle here is a snapshot.
				default:
					return fmt.Errorf("maildir/sync: adopt uid space: %w", aerr)
				}
			}
		}
		defer func() {
			if sectionProbe != nil {
				sectionProbe(int(u.sectionDir.Load()), int(u.sectionFS.Load()))
			}
		}()
		onDisk := make(map[string]*mailbox.ScanRecord, len(scanned))
		for i := range scanned {
			if scanned[i].Filename != "" {
				onDisk[maildirBase(scanned[i].Filename)] = &scanned[i]
			}
		}

		existing, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if err != nil {
			return fmt.Errorf("maildir/sync: get messages: %w", err)
		}
		tracked := make(map[string]struct{}, len(existing))
		var restamp map[uint32][16]byte
		var zeroGUID [16]byte
		for _, m := range existing {
			if m.Filename == "" {
				continue
			}
			base := maildirBase(m.Filename)
			rec, ok := onDisk[base]
			if !ok {
				// Absent from an unlocked scan, so confirmed before the record
				// is dropped.
				if u.stillOnDisk(folder.Name, m.Filename) {
					continue
				}
				// Vanished out of band → expunge (QRESYNC tombstone).
				if err := idx.ExpungeMessage(folder.ID, m.UID); err != nil {
					return fmt.Errorf("maildir/sync: expunge %d: %w", m.UID, err)
				}
				st.Expunged++
				continue
			}
			if _, dup := tracked[base]; dup {
				// Another record already owns this message. GetMessages is
				// UID-ordered, so the lowest UID is the keeper and the rest go:
				// left in place, expunging one would unlink the shared body.
				if err := idx.ExpungeMessage(folder.ID, m.UID); err != nil {
					return fmt.Errorf("maildir/sync: expunge duplicate %d: %w", m.UID, err)
				}
				st.Expunged++
				continue
			}
			tracked[base] = struct{}{}
			if m.GUID == zeroGUID && rec.GUID != zeroGUID {
				// Stamped regardless of the backfill marker: a record imported
				// into an already-complete folder is invisible to the backfill,
				// so this is the only thing that can still give it an EMAILID.
				if restamp == nil {
					restamp = make(map[uint32][16]byte, 4)
				}
				restamp[m.UID] = rec.GUID
			}
			if rec.Filename != m.Filename {
				// Renamed out of band (a flag change moves the ":2," trailer):
				// adopt the on-disk flags and repoint the filename.
				//
				// Only if the scanned name is still on disk: writing flags from
				// a name somebody has renamed past would put a state nobody set
				// into the index. If it moved on, the index keeps what it has
				// and the next pass settles it (#1626).
				if !u.stillOnDisk(folder.Name, rec.Filename) {
					continue
				}
				if !sameFlags(rec.Flags, m.Flags) || !sameFlags(rec.Keywords, m.Keywords) {
					if err := idx.UpdateFlags(folder.ID, m.UID, rec.Flags, rec.Keywords); err != nil {
						return fmt.Errorf("maildir/sync: update flags %d: %w", m.UID, err)
					}
				}
				if err := idx.UpdateFilename(folder.ID, m.UID, rec.Filename); err != nil {
					return fmt.Errorf("maildir/sync: update filename %d: %w", m.UID, err)
				}
				st.Updated++
			}
		}

		for i := range scanned {
			rec := &scanned[i]
			if rec.Filename == "" {
				continue
			}
			base := maildirBase(rec.Filename)
			if _, ok := tracked[base]; ok {
				continue
			}
			// In cur/ and still there: the scan was unlocked, and a record for
			// a message since expunged -- or one still in new/ -- serves a uid
			// whose body cannot be read.
			if !u.inCurDir(folder.Name, rec.Filename) {
				continue
			}
			// Claim the base before appending: a scan that reports one message
			// twice (the same base left in both new/ and cur/) would otherwise
			// get two records, and expunging either would delete the shared file.
			tracked[base] = struct{}{}
			m := &mailbox.MessageMeta{
				Filename:     rec.Filename,
				Size:         rec.Size,
				VSize:        rec.VSize,
				InternalDate: rec.InternalDate,
				Flags:        rec.Flags,
				Keywords:     rec.Keywords,
				GUID:         rec.GUID,
			}
			// The uidlist already says which UID this file has -- it is the
			// same file this implementation and the other one both write, and
			// it is read here for its GUIDs already. Using its number keeps a
			// client's cache valid across a takeover; allocating a fresh one
			// makes every client refetch every mailbox (#1593).
			//
			// Only for a file the list knows. One delivered by an MDA since is
			// not in it, and that is the ordinary case this path was written
			// for: it gets the next UID, as before.
			if uid, known := u.UIDFor(folder.Name, rec.Filename); known && adopted {
				m.UID = uid
				if err := idx.AppendMessage(folder.ID, m); err != nil {
					return fmt.Errorf("maildir/sync: append %s at its recorded uid %d: %w",
						rec.Filename, uid, err)
				}
			} else if err := idx.AllocateAndAppend(folder.ID, m); err != nil {
				return fmt.Errorf("maildir/sync: append %s: %w", rec.Filename, err)
			}
			st.Imported++
		}
		if len(restamp) > 0 {
			if err := idx.SetGUIDs(folder.ID, restamp); err != nil {
				return fmt.Errorf("maildir/sync: restamp guids: %w", err)
			}
			// Not counted as a change: the client's view is the same, only the
			// internal id stops being all-zero.
			slog.Info("maildir: stamped missing message GUIDs",
				"folder", folder.Name, "count", len(restamp))
		}
		return nil
	})
	st.Changed = st.Imported > 0 || st.Expunged > 0 || st.Updated > 0
	return st, err
}

// sameFlags reports whether two flag sets are equal ignoring order.
func sameFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, f := range a {
		seen[f]++
	}
	for _, f := range b {
		seen[f]--
		if seen[f] < 0 {
			return false
		}
	}
	return true
}

// SyncToken returns an opaque token over the folder's cur/ and new/ mtime and
// size. Unchanged since the previous SELECT means nothing was delivered,
// removed or renamed, so the caller may skip the reconcile scan and its lock.
// An empty token (both dirs missing or unstattable) forces a reconcile.
//
// A directory whose mtime is within the current wall-clock second is "dirty":
// on coarse (1 s) mtime granularity a second same-tick change would not move
// the mtime, so the token embeds a per-call nonce to force a reconcile until
// the directory settles. This is the classic maildir same-second dirty-sync
// rule.
//
// Over NFS a stale attribute-cache mtime only delays visibility until the next
// changed token, never corrupts; keep attribute-cache TTLs short.
func (u *userMailbox) SyncToken(folder string) string {
	base := u.folderPath(folder)
	now := time.Now()
	var b strings.Builder
	dirty := false
	for _, sub := range []string{"cur", "new"} {
		fi, err := os.Stat(filepath.Join(base, sub))
		if err != nil {
			continue
		}
		mt := fi.ModTime()
		fmt.Fprintf(&b, "%s=%d/%d;", sub, mt.UnixNano(), fi.Size())
		if now.Sub(mt) < time.Second {
			dirty = true
		}
	}
	if dirty {
		fmt.Fprintf(&b, "dirty=%d", now.UnixNano())
	}
	return b.String()
}

// ---- uidlist ---------------------------------------------------------------

// On-disk filenames. The legacy name is renamed to UIDListFileName on first
// access, so subsequent runs see only the yarilo file.
// keywordsFileName is their keyword file: the mapping from the letters a
// filename carries to the names they stand for. Read, not written -- what we
// write is #1601.
const keywordsFileName = "dovecot-keywords"

const (
	UIDListFileName       = "yarilo-uidlist"
	LegacyUIDListFileName = "dovecot-uidlist"
)

func (u *userMailbox) uidListPath(folder string) string {
	return filepath.Join(u.controlFolderPath(folder), UIDListFileName)
}

// migrateLegacyUIDList renames the legacy uidlist file (LegacyUIDListFileName)
// to yarilo-uidlist when the yarilo file is absent. Idempotent.
func (u *userMailbox) migrateLegacyUIDList(folder string) error {
	dst := u.uidListPath(folder)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	src := filepath.Join(u.folderPath(folder), LegacyUIDListFileName)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("maildir: legacy uidlist stat: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		// Called from paths holding no lock, so the loser of a race sees the
		// source already gone -- that is the migration having happened (#1626).
		if _, serr := os.Stat(dst); serr == nil {
			return nil
		}
		return fmt.Errorf("maildir: legacy uidlist rename: %w", err)
	}
	return nil
}

func (u *userMailbox) readUIDList(folder string) (map[string]uint32, error) {
	if err := u.migrateLegacyUIDList(folder); err != nil {
		return nil, err
	}
	path := u.uidListPath(folder)

	fi, statErr := os.Stat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return make(map[string]uint32), nil
	}
	if statErr != nil {
		return nil, statErr
	}

	if m, ok := u.folderCacheFor(folder).snapshotUIDs(fi.ModTime(), fi.Size()); ok {
		return m, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]uint32)
	var guids map[string][16]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "3 V") {
			continue
		}
		sep := strings.Index(line, " :")
		if sep < 0 {
			continue
		}
		filename := line[sep+2:]
		parts := strings.Fields(line[:sep])
		if len(parts) == 0 {
			continue
		}
		uid64, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			continue
		}
		// The key is the base name -- everything before the first ':' -- because
		// that is what survives a flag change, and because it is what the other
		// implementation writes: it cuts the name at the info separator before
		// recording it. We used to
		// write the whole name; normalising on read keeps those files working
		// and leaves one rule over the file instead of two (#1593).
		m[maildirBase(filename)] = uint32(uid64)
		// Optional "G<hex>" field: an explicit GUID that must win over the
		// name-derived one. A later record for the same file supersedes.
		for _, fld := range parts[1:] {
			if len(fld) != 1+2*16 || fld[0] != 'G' {
				continue
			}
			raw, decErr := hex.DecodeString(fld[1:])
			if decErr != nil {
				continue
			}
			if guids == nil {
				guids = make(map[string][16]byte)
			}
			var g [16]byte
			copy(g[:], raw)
			guids[maildirBase(filename)] = g
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	u.folderCacheFor(folder).storeUIDs(m, guids, fi.ModTime(), fi.Size())
	return m, nil
}

// folderCacheFor returns this folder's cache entry, creating it if needed.
// Under the in-process mutex, because the reconcile's scan reaches it holding
// no mailbox lock (#1626); that mutex is not the cross-process one.
func (u *userMailbox) folderCacheFor(folder string) *folderCache {
	u.cacheMu.Lock()
	defer u.cacheMu.Unlock()
	if u.cache == nil {
		u.cache = make(map[string]*folderCache)
	}
	c, ok := u.cache[folder]
	if !ok {
		c = &folderCache{}
		u.cache[folder] = c
	}
	return c
}

// ---- path helpers ----------------------------------------------------------

// folderDiskName maps a logical folder name to the on-disk directory component:
// storage-name escaping, then modified-UTF-7 when legacy encoding is set.
//
// It does not normalise. NFC is applied once at the name-entry boundary
// (mailbox.NormalizeName), so folder arrives already in its final form and the
// order of NFC against escaping -- which lived here and in FolderSubpath both,
// and broke a name with a combining mark straight after an escape -- no longer
// exists to get wrong (#1113).
func (u *userMailbox) folderDiskName(folder string) string {
	folder = mailbox.EscapeLogicalName(folder, u.separator, ".", u.escapeChar)
	if !u.listUTF8 {
		folder = toModUTF7(folder)
	}
	return folder
}

// checkName refuses a folder name that would resolve outside its own folder.
//
// INBOX is exempt because it names the root deliberately; every other name that
// resolves there does so by accident, and the accident removes a mailbox.
func (u *userMailbox) checkName(folder string) error {
	if folder == "INBOX" {
		return nil
	}
	return mailbox.ValidateFolderName(folder, u.separator)
}

// folderPath maps a folder name to its directory.
//
// A name that would resolve to the mailbox root or above it is mapped to a
// path that cannot exist instead. Refusing here rather than only in the
// mutating operations is what makes it safe: the same names read another
// account's mail on a deployment whose IMAP separator is "." — the rewrite to
// the on-disk separator neutralises "../" only while the two differ, which is
// configuration rather than a guarantee (#1063).
//
// Every caller then fails with "no such file or directory", which is the right
// answer for a mailbox that cannot exist. The operations that destroy data
// check the name explicitly as well, so they refuse with a message naming the
// cause rather than a missing path.
func (u *userMailbox) folderPath(folder string) string {
	if folder != "INBOX" {
		if err := mailbox.ValidateFolderName(folder, u.separator); err != nil {
			return filepath.Join(u.mailPath, invalidFolderMarker)
		}
	}
	if folder == "INBOX" {
		if u.explicitMailPath {
			return u.inboxPath
		}
		return filepath.Join(u.home, "INBOX")
	}
	return filepath.Join(u.mailPath, mailbox.FolderSubpath("maildir", folder, u.folderDiskName(folder), u.separator))
}

// controlFolderPath returns the directory for per-folder control files
// (yarilo-uidlist): under controlDir when CONTROL= is set, else co-located
// with the folder.
// invalidFolderMarker is the directory an invalid name resolves to. It is not
// a legal maildir++ folder name — a folder is ".name" — so it cannot collide
// with a real one, and it does not exist, so every read and write fails.
const invalidFolderMarker = "invalid-folder-name"

func (u *userMailbox) controlFolderPath(folder string) string {
	if folder != "INBOX" {
		if err := mailbox.ValidateFolderName(folder, u.separator); err != nil {
			return filepath.Join(u.mailPath, invalidFolderMarker)
		}
	}
	sub := mailbox.FolderSubpath("maildir", folder, u.folderDiskName(folder), u.separator)
	if u.controlDir != "" {
		if folder == "INBOX" {
			return filepath.Join(u.controlDir, "INBOX")
		}
		return filepath.Join(u.controlDir, sub)
	}
	if folder == "INBOX" {
		if u.explicitMailPath {
			return u.inboxPath
		}
		return filepath.Join(u.home, "INBOX")
	}
	return filepath.Join(u.mailPath, sub)
}

// ---- flag helpers ----------------------------------------------------------

// WriteFlags renames a message so its name carries the flags and keywords it
// now has, which is where a maildir keeps that state. Within one directory, so
// the rename is atomic; a new keyword takes the first free letter and never
// renumbers one in use, since that changes what every existing name means. A
// name that would not change is returned as it is.
func (u *userMailbox) WriteFlags(folder, filename string, flags, keywords []string) (string, error) {
	var newName string
	err := u.withMailboxLockSite(folder, lockSiteWriteFlags, func() error {
		letters, lerr := u.keywordLettersLocked(folder, keywords)
		if lerr != nil {
			return lerr
		}
		var werr error
		newName, werr = u.writeFlagsLocked(folder, filename, flags, letters)
		return werr
	})
	if err != nil {
		return filename, err
	}
	return newName, nil
}

// WriteFlagsMulti records a whole command's flag writes under one acquisition of
// the folder lock, reading the keyword file once and rewriting it at most once
// for the batch; per message it did both per message (#1623). Best effort stays
// per message: one failure is reported against its uid, the rest are written.
func (u *userMailbox) WriteFlagsMulti(folder string, writes []mailbox.FlagWrite) []mailbox.FlagWriteResult {
	out := make([]mailbox.FlagWriteResult, len(writes))
	for i := range writes {
		out[i] = mailbox.FlagWriteResult{UID: writes[i].UID, Filename: writes[i].Filename}
	}
	err := u.withMailboxLockSite(folder, lockSiteWriteFlagsBulk, func() error {
		t := u.loadKeywordTableLocked(folder)
		added := false
		for i := range writes {
			if t.allocate(u, folder, writes[i].Keywords) {
				added = true
			}
		}
		if added {
			if werr := u.writeKeywordFileLocked(folder, t.names); werr != nil {
				return werr
			}
		}
		for i := range writes {
			name, werr := u.writeFlagsLocked(folder, writes[i].Filename,
				writes[i].Flags, t.letters(writes[i].Keywords))
			if werr != nil {
				out[i].Err = werr
				continue
			}
			out[i].Filename = name
		}
		return nil
	})
	if err != nil {
		// The lock itself: nothing in the batch was written.
		for i := range out {
			out[i].Err = err
		}
	}
	return out
}

// writeFlagsLocked renames one file, with its keyword letters already resolved.
// The caller holds the folder lock.
func (u *userMailbox) writeFlagsLocked(folder, filename string, flags []string, letters string) (string, error) {
	want := renameWithFlags(filename, encodeFlags(flags)+letters)
	if want == filename {
		return filename, nil
	}
	dir := u.folderPath(folder)
	for _, sub := range []string{"cur", "new"} {
		from := filepath.Join(dir, sub, filename)
		if _, serr := os.Stat(from); serr != nil {
			continue
		}
		if rerr := os.Rename(from, filepath.Join(dir, sub, want)); rerr != nil {
			return filename, fmt.Errorf("maildir/flags: rename %s: %w", filename, rerr)
		}
		return want, nil
	}
	// The file is not where the index says it is. Left to the reconcile pass,
	// which is what notices a message that moved or went; failing here would
	// turn a flag change into an error a client cannot act on.
	return filename, nil
}

// renameWithFlags returns the filename with its ":2," info part replaced.
func renameWithFlags(filename, info string) string {
	base := filename
	if i := strings.Index(filename, ":2,"); i >= 0 {
		base = filename[:i]
	}
	return base + ":2," + info
}

// keywordLetters is keywordLettersLocked under the folder lock, for callers
// that do not already hold it -- Save writes into its own tmp file and takes
// the lock for nothing else.
func (u *userMailbox) keywordLetters(folder string, keywords []string) (string, error) {
	if len(keywords) == 0 {
		return "", nil
	}
	var letters string
	err := u.withMailboxLockSite(folder, lockSiteKeywords, func() error {
		var lerr error
		letters, lerr = u.keywordLettersLocked(folder, keywords)
		return lerr
	})
	return letters, err
}

// keywordLettersLocked returns the letters standing for keywords, adding any
// the folder's keyword file does not name yet. Sorted, so a name is one string
// regardless of the order the keywords arrived in.
func (u *userMailbox) keywordLettersLocked(folder string, keywords []string) (string, error) {
	if len(keywords) == 0 {
		return "", nil
	}
	t := u.loadKeywordTableLocked(folder)
	if t.allocate(u, folder, keywords) {
		if err := u.writeKeywordFileLocked(folder, t.names); err != nil {
			return "", err
		}
	}
	return t.letters(keywords), nil
}

// sectionProbe reports what the reconcile's critical section did to the
// filesystem, so "no directory walk under the lock" is counted rather than
// described (#1626).
var sectionProbe func(dirReads, stats int)

// cleanProbe reports how many records the clean-check compared, so a test
// asserting "no lock taken" knows it ran over a real folder (#1630).
var cleanProbe func(records int)

// movePhaseProbe reports whether the new/ move phase took the lock, so a test
// asserting "one acquisition, not two" knows the phase was skipped rather than
// assuming it (#1630).
var movePhaseProbe func(taken bool)

// afterScan runs between the unlocked scan and the apply, so a test can make
// the scan stale on purpose -- which is the only way to exercise the rule that
// a name the scan saw may already have moved on (#1626).
var afterScan func()

// keywordFileRead is called with the keyword file's path on every read of it.
// Nil in a running server; a test sets it to count reads, which is how "once
// per batch" is asserted rather than described (#1623).
var keywordFileRead func(path string)

// keywordTable is a folder's keyword file held in memory, so a batch reads and
// rewrites it once rather than per message (#1623).
type keywordTable struct {
	names  map[byte]string
	byName map[string]byte
}

func (u *userMailbox) loadKeywordTableLocked(folder string) *keywordTable {
	names := u.keywordNames(folder)
	if names == nil {
		// No keyword file yet: this folder is about to have its first keyword.
		names = make(map[byte]string)
	}
	byName := make(map[string]byte, len(names))
	for letter, name := range names {
		byName[name] = letter
	}
	return &keywordTable{names: names, byName: byName}
}

// allocate gives every keyword a letter it does not already have, and reports
// whether the table changed.
func (t *keywordTable) allocate(u *userMailbox, folder string, keywords []string) bool {
	added := false
	for _, kw := range keywords {
		if _, ok := t.byName[kw]; ok {
			continue
		}
		letter, ok := firstFreeKeywordLetter(t.names)
		if !ok {
			// Twenty-six is all a maildir name can carry. The keyword stays in
			// the index with no letter; dropping it there would lose it.
			slog.Warn("maildir: no free keyword letter left in this folder",
				"user", u.username, "folder", folder, "keyword", kw)
			continue
		}
		t.names[letter] = kw
		t.byName[kw] = letter
		added = true
	}
	return added
}

func (t *keywordTable) letters(keywords []string) string {
	letters := make([]byte, 0, len(keywords))
	for _, kw := range keywords {
		if letter, ok := t.byName[kw]; ok {
			letters = append(letters, letter)
		}
	}
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })
	return string(letters)
}

// firstFreeKeywordLetter returns the lowest letter no keyword holds.
//
// The first free one rather than the next after the highest: the reference
// fills a hole left by a removed keyword the same way, and either choice has to
// avoid renumbering, which is what would change the meaning of every filename
// already written.
func firstFreeKeywordLetter(names map[byte]string) (byte, bool) {
	for c := byte('a'); c <= 'z'; c++ {
		if _, taken := names[c]; !taken {
			return c, true
		}
	}
	return 0, false
}

// writeKeywordFileLocked rewrites the folder's keyword file from the mapping.
func (u *userMailbox) writeKeywordFileLocked(folder string, names map[byte]string) error {
	letters := make([]byte, 0, len(names))
	for c := range names {
		letters = append(letters, c)
	}
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })

	var b strings.Builder
	for _, c := range letters {
		fmt.Fprintf(&b, "%d %s\n", c-'a', names[c])
	}
	path := filepath.Join(u.folderPath(folder), keywordsFileName)
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("maildir/keywords: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("maildir/keywords: commit: %w", err)
	}
	return nil
}

func encodeFlags(flags []string) string {
	set := make(map[byte]bool)
	for _, f := range flags {
		switch strings.ToLower(f) {
		case `\answered`:
			set['R'] = true
		case `\deleted`:
			set['T'] = true
		case `\draft`:
			set['D'] = true
		case `\flagged`:
			set['F'] = true
		case `\seen`:
			set['S'] = true
		}
	}
	order := []byte{'D', 'F', 'R', 'S', 'T'}
	var b strings.Builder
	for _, c := range order {
		if set[c] {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseSizeInfo(name string) (phys, virt uint32, hasPhys, hasVirt bool) {
	prefix := name
	if i := strings.Index(name, ":2,"); i >= 0 {
		prefix = name[:i]
	}
	for _, kv := range strings.Split(prefix, ",") {
		switch {
		case strings.HasPrefix(kv, "S="):
			if n, err := strconv.ParseUint(kv[2:], 10, 32); err == nil {
				phys = uint32(n)
				hasPhys = true
			}
		case strings.HasPrefix(kv, "W="):
			if n, err := strconv.ParseUint(kv[2:], 10, 32); err == nil {
				virt = uint32(n)
				hasVirt = true
			}
		}
	}
	return
}

// keywordNames reads the folder's keyword file: the mapping from the letters a
// maildir filename carries to the names they stand for. One line per keyword,
// "<index> <name>", the letter being 'a'+index. A letter the file does not name
// stays unresolved: inventing a name for it served a client something nothing
// on disk ever said (#1600).
func (u *userMailbox) keywordNames(folder string) map[byte]string {
	path := filepath.Join(u.folderPath(folder), keywordsFileName)
	if keywordFileRead != nil {
		keywordFileRead(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck
	out := make(map[byte]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		idxStr, name, found := strings.Cut(strings.TrimRight(sc.Text(), "\r"), " ")
		if !found || name == "" {
			continue
		}
		idx, cerr := strconv.ParseUint(idxStr, 10, 32)
		if cerr != nil || idx >= 26 {
			continue
		}
		out['a'+byte(idx)] = name
	}
	return out
}

func decodeFlags(filename string) (flags, keywords []string) {
	return decodeFlagsWith(filename, nil)
}

// decodeFlagsWith reads a filename's flags, resolving keyword letters through
// the folder's keyword file when it names them.
func decodeFlagsWith(filename string, names map[byte]string) (flags, keywords []string) {
	idx := strings.Index(filename, ":2,")
	if idx < 0 {
		return nil, nil
	}
	info := filename[idx+3:]
	for _, c := range info {
		switch c {
		case 'D':
			flags = append(flags, `\Draft`)
		case 'F':
			flags = append(flags, `\Flagged`)
		case 'R':
			flags = append(flags, `\Answered`)
		case 'S':
			flags = append(flags, `\Seen`)
		case 'T':
			flags = append(flags, `\Deleted`)
		default:
			if c >= 'a' && c <= 'z' {
				// Only what the folder's keyword file names. A letter with no
				// line of its own is a keyword whose name is not recorded
				// anywhere, and inventing one puts a name in front of a client
				// that nothing on disk ever said (#1600).
				if name, ok := names[byte(c)]; ok {
					keywords = append(keywords, name)
				}
			}
		}
	}
	return
}

func randomGUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%032x", b)
}

// UIDSpace reports the UIDVALIDITY and next UID a folder's uidlist records, and
// whether it had them.
//
// The header line is "3 V<uidvalidity> N<next uid> G<guid>", written by this
// implementation and by the one whose file this may have been -- the format is
// the same, which is why the file is adopted under our name rather than
// converted. What was not adopted until now is what it says: the numbers were
// parsed past on every read, so a store taken over got a fresh UID space and
// every client refetched every mailbox (#1593).
func (u *userMailbox) UIDSpace(folder string) (uidValidity, nextUID uint32, ok bool) {
	if err := u.migrateLegacyUIDList(folder); err != nil {
		return 0, 0, false
	}
	f, err := os.Open(u.uidListPath(folder))
	if err != nil {
		return 0, 0, false
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) == 0 || fields[0] != "3" {
		return 0, 0, false
	}
	for _, fld := range fields[1:] {
		if len(fld) < 2 {
			continue
		}
		n, cerr := strconv.ParseUint(fld[1:], 10, 32)
		if cerr != nil {
			continue
		}
		switch fld[0] {
		case 'V':
			uidValidity = uint32(n)
		case 'N':
			nextUID = uint32(n)
		}
	}
	return uidValidity, nextUID, uidValidity != 0
}

// UIDFor returns the UID a folder's uidlist records for one file.
func (u *userMailbox) UIDFor(folder, filename string) (uint32, bool) {
	m, err := u.readUIDList(folder)
	if err != nil {
		return 0, false
	}
	uid, ok := m[maildirBase(filename)]
	return uid, ok
}

// splitFlagsAndKeywords separates a caller's one list into the two the name
// records differently: system flags as their own letters, keywords through the
// folder's keyword file.
func splitFlagsAndKeywords(all []string) (flags, keywords []string) {
	for _, f := range all {
		if strings.HasPrefix(f, `\`) {
			flags = append(flags, f)
		} else {
			keywords = append(keywords, f)
		}
	}
	return flags, keywords
}

// stillOnDisk reports whether a name the unlocked scan produced is still on
// disk. One stat, and only for a record the pass changes, so the section's cost
// follows the changes and not the size of the folder (#1626).
func (u *userMailbox) stillOnDisk(folder, filename string) bool {
	if u.inSection.Load() > 0 {
		u.sectionFS.Add(1)
	}
	dir := u.folderPath(folder)
	for _, sub := range []string{"cur", "new"} {
		if _, err := os.Lstat(filepath.Join(dir, sub, filename)); err == nil {
			return true
		}
	}
	return false
}

// hasNewMail reports whether new/ holds anything to move: one directory read,
// unlocked, deciding only whether a round trip is worth taking.
//
// Unreadable answers yes, so the locked phase runs and fails loudly: answering
// no would skip the move for good on a faulty mount, and since only cur/ is
// imported from, the mail would sit invisible with nothing reported (#1630).
func (u *userMailbox) hasNewMail(folder string) bool {
	entries, err := os.ReadDir(filepath.Join(u.folderPath(folder), "new"))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// inCurDir reports whether a scanned name is in cur/. Only those are imported:
// Fetch opens cur/ alone, so a record naming a file in new/ has an unreadable
// body until the next pass moves it (#1630).
func (u *userMailbox) inCurDir(folder, filename string) bool {
	if u.inSection.Load() > 0 {
		u.sectionFS.Add(1)
	}
	_, err := os.Lstat(filepath.Join(u.folderPath(folder), "cur", filename))
	return err == nil
}

// reconcileIsClean reports whether the scan and the index already agree, so the
// apply phase has nothing to do and need not take its lock. Every difference is
// work: an unknown file, a vanished one, a renamed one -- which in maildir is
// what a flag change is -- a duplicate record, and a record with no GUID where
// storage has one. It answers false whenever it cannot be sure, including on a
// folder with no records, which may still be waiting to adopt a UID space.
func (u *userMailbox) reconcileIsClean(idx mailbox.UserIndex, folder *mailbox.Folder, scanned []mailbox.ScanRecord) bool {
	reader, ok := idx.(mailbox.UnlockedReader)
	if !ok {
		return false
	}
	existing, err := reader.GetMessagesUnlocked(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(existing) == 0 {
		return false
	}
	var zeroGUID [16]byte
	byName := make(map[string]*mailbox.MessageMeta, len(existing))
	for _, m := range existing {
		if m.Filename == "" {
			return false
		}
		if _, dup := byName[m.Filename]; dup {
			// Two records on one file: the pass collapses them, which is work.
			return false
		}
		byName[m.Filename] = m
	}
	// Counted, not just matched: a record for a file the scan does not report
	// is work even when every scanned file is known.
	if len(byName) != len(scanned) {
		return false
	}
	for i := range scanned {
		if scanned[i].Filename == "" {
			return false
		}
		m, known := byName[scanned[i].Filename]
		if !known {
			return false
		}
		if m.GUID == zeroGUID && scanned[i].GUID != zeroGUID {
			// The record has no identity and the storage has one for it:
			// stamping it is the only thing that ever will.
			return false
		}
	}
	if cleanProbe != nil {
		cleanProbe(len(existing))
	}
	return true
}
