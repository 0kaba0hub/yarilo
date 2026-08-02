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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Backend is the Maildir MailboxBackend factory. Holds only
// process-wide state (hostname, pid, counter); per-user state lives in userMailbox.
type Backend struct {
	hostname     string
	pid          int
	counter      atomic.Uint64
	locker       locks.Locker
	writeSem     chan struct{} // nil = unlimited
	listUTF8     bool          // true = UTF-8 on disk (default); false = modified-UTF-7
	normalizeNFC bool          // true = NFC-normalise folder names (default)
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

// WithNormalizeNFC enables Unicode NFC normalization of folder names
// before storage and comparison. Default true.
func WithNormalizeNFC(v bool) Option { return func(b *Backend) { b.normalizeNFC = v } }

// New creates a Maildir backend.
func New(opts ...Option) *Backend {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	b := &Backend{
		hostname:     hostname,
		pid:          os.Getpid(),
		listUTF8:     true,
		normalizeNFC: true,
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
		username:         u.Username,
		owner:            makeOwner(u),
		listUTF8:         b.listUTF8,
		normalizeNFC:     b.normalizeNFC,
	}
}

// folderCache holds mtime-validated in-memory state for one maildir folder.
type folderCache struct {
	uidMap   map[string]uint32
	guidMap  map[string][16]byte // explicit GUID overrides; empty for name-derived GUIDs
	uidMtime time.Time
	uidSize  int64
	entries  []os.DirEntry
	dirMtime time.Time
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
	username         string
	owner            string                  // <process>/<pid>/<user> — passed to yarilo-locks for BUSY diagnostics
	listUTF8         bool                    // mirrors Backend.listUTF8
	normalizeNFC     bool                    // mirrors Backend.normalizeNFC
	mu               sync.Mutex              // in-process fast-path; cross-process barrier is b.locker
	cache            map[string]*folderCache // keyed by folder name; lazy-initialised
}

// makeOwner builds the yarilo-locks owner string
// "<process>/<pid>/<user>[/<sid>]"; the session ID disambiguates concurrent
// sessions for the same user.
func makeOwner(u *mailbox.UserInfo) string {
	proc := "yarilo"
	if len(os.Args) > 0 {
		proc = filepath.Base(os.Args[0])
	}
	if u.SessionID != "" {
		return fmt.Sprintf("%s/%d/%s/%s", proc, os.Getpid(), u.Username, u.SessionID)
	}
	return fmt.Sprintf("%s/%d/%s", proc, os.Getpid(), u.Username)
}

// withMailboxLock runs fn under the in-process mutex, then the cross-process
// yarilo-locks X lock (only when b.locker is non-nil).
func (u *userMailbox) withMailboxLock(folder string, fn func() error) error {
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
	return nil
}

// Create provisions the cur/new/tmp triplet for a folder under the X lock.
func (u *userMailbox) Create(folder string) error {
	return u.withMailboxLock(folder, func() error {
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
	return u.withMailboxLock(folder, func() error {
		return os.RemoveAll(u.folderPath(folder))
	})
}

// Rename renames a folder on disk, holding the X lock on both names in
// lexicographic order so concurrent Renames cannot deadlock.
func (u *userMailbox) Rename(oldName, newName string) error {
	return u.withTwoMailboxLocks(oldName, newName, func() error {
		return os.Rename(u.folderPath(oldName), u.folderPath(newName))
	})
}

// withTwoMailboxLocks takes both per-folder X locks in lexicographic order.
// Same ordering as the index side so a Rename rippling through both backends
// cannot deadlock.
func (u *userMailbox) withTwoMailboxLocks(folderA, folderB string, fn func() error) error {
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
		defer func() { _ = u.b.locker.Unlock(ctx, lkB.ID) }()
	}
	return fn()
}

// Save streams r into tmp/ then atomically renames into cur/. uid comes from
// UserIndex.AllocateUID; Maildir does not encode it in the filename, so the
// uid→filename mapping is appended inline to the yarilo-uidlist sidecar for
// later List() / Fetch() resolution.
func (u *userMailbox) Save(folder string, r io.Reader, uid uint32, _ int64, flags []string, guid [16]byte) (string, uint32, [16]byte, error) {
	var noGUID [16]byte
	if u.b.writeSem != nil {
		u.b.writeSem <- struct{}{}
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

	flagStr := encodeFlags(flags)
	// ,S=<phys>,W=<virt> before :2,<flags> so List() reports both sizes
	// without reading the body.
	finalName := fmt.Sprintf("%s,S=%d,W=%d:2,%s", basename, sc.phys, sc.phys+sc.lfNoCR, flagStr)

	// The base name is fresh, so the derived GUID is unique. A caller asking for
	// a specific GUID (identity-preserving migration) gets an explicit uidlist
	// override; the common paths pass zero and ride on the derivation.
	effGUID := guidFromBase(finalName)
	override := guid != noGUID && guid != effGUID
	if override {
		effGUID = guid
	}

	if err := u.withMailboxLock(folder, func() error {
		dstPath := filepath.Join(folderPath, "cur", finalName)
		if err := os.Rename(tmpPath, dstPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("maildir: rename to cur: %w", err)
		}
		u.folderCacheFor(folder).entries = nil
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
// GUID — and therefore EMAILID — is unchanged (RFC 8474). Only the ":2,"
// trailer and the folder change. A base-name collision in the destination is
// resolved with a fresh name plus an explicit uidlist GUID override, which is
// the one case where the derivation alone would break identity.
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
	err := u.withTwoMailboxLocks(srcFolder, dstFolder, func() error {
		srcPath := filepath.Join(u.folderPath(srcFolder), "cur", filename)
		dstDir := filepath.Join(u.folderPath(dstFolder), "cur")
		dstPath := filepath.Join(dstDir, newName)
		override := outGUID != guidFromBase(newName)
		if _, err := os.Lstat(dstPath); err == nil {
			// Base name taken in the destination: mint a fresh one and pin the
			// original GUID explicitly so the id still survives.
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
		u.folderCacheFor(srcFolder).entries = nil
		u.folderCacheFor(dstFolder).entries = nil
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
	// v3 record: "<uid> [G<guid>] :<filename>". The GUID field is written only
	// when it must differ from the name-derived one; readers that predate it
	// skip unknown fields.
	if guidOverride {
		_, err = fmt.Fprintf(f, "%d G%s :%s\n", uid, hex.EncodeToString(guid[:]), filename)
	} else {
		_, err = fmt.Fprintf(f, "%d :%s\n", uid, filename)
	}
	if err != nil {
		return err
	}

	// Update the cache inline so the next readUIDList skips the file.
	if fi, statErr := f.Stat(); statErr == nil {
		c := u.folderCacheFor(folder)
		if c.uidMap == nil {
			c.uidMap = make(map[string]uint32)
		}
		c.uidMap[filename] = uid
		if guidOverride {
			if c.guidMap == nil {
				c.guidMap = make(map[string][16]byte)
			}
			c.guidMap[filename] = guid
		}
		c.uidMtime = fi.ModTime()
		c.uidSize = fi.Size()
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
	u.folderCacheFor(folder).entries = nil
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
	var entries []os.DirEntry
	if c.entries != nil && dirFi.ModTime().Equal(c.dirMtime) {
		entries = c.entries
	} else {
		var err error
		entries, err = os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		c.entries = entries
		c.dirMtime = dirFi.ModTime()
	}

	uidMap, err := u.readUIDList(folder)
	if err != nil {
		return nil, err
	}

	var msgs []*mailbox.MessageMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		flags, keywords := decodeFlags(name)
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
		uid := uidMap[name]
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
		if u.normalizeNFC {
			logical = nfcNormalize(logical)
		}
		// maildir++ stores hierarchy flat with "."; map it back to the
		// namespace's IMAP separator (every "." is a level).
		if u.separator != "." {
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
	// Warm the uidlist cache so explicit GUID overrides win over the derived
	// value; a missing uidlist just leaves every GUID name-derived.
	_, _ = u.readUIDList(folder)
	out := make([]mailbox.ScanRecord, 0, 128)
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
			flags, keywords := decodeFlags(name)
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
				GUID:         u.guidFor(folder, name),
			}
			if len(keywords) > 0 {
				rec.Flags = append(rec.Flags, keywords...)
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
	if c := u.folderCacheFor(folder); c != nil && c.guidMap != nil {
		if g, ok := c.guidMap[filename]; ok {
			return g
		}
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

// guidFromBase derives the per-message GUID from the maildir base name.
// The base ({secs}.M{usecs}P{pid}_{seq}.{hostname}) is unique per message by
// construction and never rewritten: a flag change touches only the ":2,"
// trailer and MOVE renames across folders keeping the base. So EMAILID stays
// stable with nothing stored, and an index rebuild recomputes it. A GUID that
// must differ from the derived one (base-name collision in the destination on
// MOVE) is written to the uidlist as an explicit "G" field and wins on read.
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

// ReconcileIndex brings idx into agreement with the physical maildir under the
// mailbox lock:
//
//   - new/ is migrated into cur/ first, so an MDA delivery becomes readable.
//   - Messages match by base name, so a second MUA flipping ":2," → ":2,S"
//     keeps the UID.
//   - New files get a UID via the index's atomic allocator (no manual seed, so
//     a concurrent delivery cannot collide).
//   - A vanished tracked file is expunged incrementally (QRESYNC tombstone); a
//     tracked file renamed out of band has its stored filename and flags
//     updated in place.
//
// Tracked messages with an unchanged on-disk name are left untouched — the
// index stays authoritative for flags yarilo set, which never rename the file.
func (u *userMailbox) ReconcileIndex(idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, error) {
	var st mailbox.SyncStats
	err := u.withMailboxLock(folder.Name, func() error {
		if err := u.moveNewToCurLocked(folder.Name); err != nil {
			return err
		}
		scanned, err := u.Scan(folder.Name)
		if err != nil {
			return fmt.Errorf("maildir/sync: scan: %w", err)
		}
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
		for _, m := range existing {
			if m.Filename == "" {
				continue
			}
			base := maildirBase(m.Filename)
			rec, ok := onDisk[base]
			if !ok {
				// Vanished out of band → expunge (QRESYNC tombstone).
				if err := idx.ExpungeMessage(folder.ID, m.UID); err != nil {
					return fmt.Errorf("maildir/sync: expunge %d: %w", m.UID, err)
				}
				st.Expunged++
				continue
			}
			tracked[base] = struct{}{}
			if rec.Filename != m.Filename {
				// Renamed out of band (a flag change moves the ":2," trailer):
				// adopt the on-disk flags and repoint the filename.
				if !sameFlags(rec.Flags, m.Flags) {
					if err := idx.UpdateFlags(folder.ID, m.UID, rec.Flags, nil); err != nil {
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
			if _, ok := tracked[maildirBase(rec.Filename)]; ok {
				continue
			}
			m := &mailbox.MessageMeta{
				Filename:     rec.Filename,
				Size:         rec.Size,
				VSize:        rec.VSize,
				InternalDate: rec.InternalDate,
				Flags:        rec.Flags,
			}
			if err := idx.AllocateAndAppend(folder.ID, m); err != nil {
				return fmt.Errorf("maildir/sync: append %s: %w", rec.Filename, err)
			}
			st.Imported++
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
const (
	UIDListFileName       = "yarilo-uidlist"
	LegacyUIDListFileName = "dovecot-uidlist"
)

func (u *userMailbox) uidListPath(folder string) string {
	return filepath.Join(u.controlFolderPath(folder), UIDListFileName)
}

// migrateLegacyUIDList renames dovecot-uidlist → yarilo-uidlist when the
// yarilo file is absent. Idempotent.
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

	if c := u.folderCacheFor(folder); c.uidMap != nil &&
		fi.ModTime().Equal(c.uidMtime) && fi.Size() == c.uidSize {
		return c.uidMap, nil
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
		m[filename] = uint32(uid64)
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
			guids[filename] = g
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	c := u.folderCacheFor(folder)
	c.uidMap = m
	c.guidMap = guids
	c.uidMtime = fi.ModTime()
	c.uidSize = fi.Size()
	return m, nil
}

// folderCacheFor returns the folderCache for folder, creating it if needed.
// Caller must hold u.mu or ensure single-goroutine access.
func (u *userMailbox) folderCacheFor(folder string) *folderCache {
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

// folderDiskName maps a logical UTF-8 folder name to the on-disk directory
// component: NFC normalisation, then modified-UTF-7 when legacy encoding is set.
func (u *userMailbox) folderDiskName(folder string) string {
	if u.normalizeNFC {
		folder = nfcNormalize(folder)
	}
	if !u.listUTF8 {
		folder = toModUTF7(folder)
	}
	return folder
}

func (u *userMailbox) folderPath(folder string) string {
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
func (u *userMailbox) controlFolderPath(folder string) string {
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

func decodeFlags(filename string) (flags, keywords []string) {
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
				keywords = append(keywords, fmt.Sprintf("kw_%c", c))
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
