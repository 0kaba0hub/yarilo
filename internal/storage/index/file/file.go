// Package file is the per-folder index implementation that
// underlies every yarilo storage driver (maildir, dbox, mdbox).
//
// It is a thin adapter on top of internal/storage/mailindex; the
// on-disk format is byte-for-byte the canonical mail-index v7.3.
// The yarilo-specific .names sidecar is transitional (sdbox
// encodes the UID in the filename, mdbox uses map_uid instead).
//
// The package exposes the Backend / OpenUser / UserIndex surface.
package file

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Backend is the per-process factory for fileindex handles. It
// holds only process-wide state; per-user state lives in
// userIndex (created by OpenUser).
const (
	defaultLogCompactMinBytes   int64 = 32 * 1024   // 32 KiB
	defaultLogCompactMaxBytes   int64 = 1024 * 1024 // 1 MiB
	defaultLogCompactMinAgeSecs int   = 300         // 5 min
)

// sidecarTmpSeq gives concurrent sidecar-file writers (saveNames,
// ensureLogStub) unique tmp names, so a shared "<path>.tmp" race cannot make
// the loser's os.Rename fail with ENOENT after the winner consumes the source.
var sidecarTmpSeq atomic.Uint64

// sidecarTmpPath returns a unique working path for a sidecar rewrite of dst.
// When volatileDir is set the tmp file goes there (fast local volatile volume)
// rather than next to dst on the shared mail volume; the caller must then stage
// it back onto dst's filesystem before the final rename, since os.Rename cannot
// cross devices.
func sidecarTmpPath(dst, volatileDir string) string {
	suffix := fmt.Sprintf(".tmp.%d.%d", os.Getpid(), sidecarTmpSeq.Add(1))
	if volatileDir != "" {
		return filepath.Join(volatileDir, filepath.Base(dst)+suffix)
	}
	return dst + suffix
}

// sidecarStagePath returns a same-filesystem-as-dst staging path, used only
// when the tmp file was written to a different device (volatileDir set).
func sidecarStagePath(dst string) string {
	return fmt.Sprintf("%s.stage.%d.%d", dst, os.Getpid(), sidecarTmpSeq.Add(1))
}

// sidecarCommit renames tmp onto dst, staging via a same-device copy first
// when volatileDir put tmp on a different filesystem than dst (os.Rename
// cannot cross devices). Always cleans up its own scratch files.
func sidecarCommit(tmp, dst, volatileDir string) error {
	if volatileDir == "" {
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
	stage := sidecarStagePath(dst)
	if err := copySidecarTmp(tmp, stage); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(tmp)
	if err := os.Rename(stage, dst); err != nil {
		_ = os.Remove(stage)
		return err
	}
	return nil
}

func copySidecarTmp(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

type Backend struct {
	locker locks.Locker

	// noCreate makes OpenFolder fail instead of initialising a folder whose
	// index is absent: a fabricated index reads as a healthy empty folder and
	// hides whatever the store really held.
	noCreate bool

	logCompactMinBytes int64
	logCompactMaxBytes int64
	logCompactMinAge   time.Duration

	// users caches one userIndex per username so all sessions for the same
	// user share a single in-process index state, serialising on fs.mu rather
	// than contending the cross-process Redis mailbox lock within a pod.
	usersMu sync.Mutex
	users   map[string]*refUserIndex
}

// refUserIndex is the cache entry: a shared userIndex plus a reference
// count tracking how many active sessions (userHandle values) use it.
type refUserIndex struct {
	ui   *userIndex
	refs int // protected by Backend.usersMu
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend. Every
// folder mutation takes the cross-process X lock via
// mailindex.WithSyncLock before reading/writing the .index file.
// Nil disables cross-process locking — kept for unit tests; never
// safe in production.
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// WithNoCreate refuses to initialise a missing folder index. Use it in any tool
// that must observe a store rather than establish one.
func WithNoCreate() Option {
	return func(b *Backend) { b.noCreate = true }
}

// WithLogCompaction configures automatic log compaction thresholds.
// minBytes / maxBytes control when rotation fires; minAge prevents
// rotation before the log reaches a minimum age. Pass 0 for minBytes
// to disable.
func WithLogCompaction(minBytes, maxBytes int64, minAge time.Duration) Option {
	return func(b *Backend) {
		b.logCompactMinBytes = minBytes
		b.logCompactMaxBytes = maxBytes
		b.logCompactMinAge = minAge
	}
}

// New constructs a Backend.
func New(opts ...Option) *Backend {
	b := &Backend{
		users:              make(map[string]*refUserIndex),
		logCompactMinBytes: defaultLogCompactMinBytes,
		logCompactMaxBytes: defaultLogCompactMaxBytes,
		logCompactMinAge:   time.Duration(defaultLogCompactMinAgeSecs) * time.Second,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u. All sessions for
// the same username share one underlying userIndex so they serialise
// on the per-folder in-process mutex rather than competing for the
// cross-process Redis mailbox lock.
// cacheKey identifies a shared userIndex. It is the username combined
// with every field that determines the on-disk index root, so the same
// user accessing distinct storage roots (personal vs shared/public
// namespaces at different locations) gets separate index state instead
// of colliding on username alone.
func cacheKey(u *mailbox.UserInfo) string {
	return strings.Join([]string{u.Username, u.IndexDir, u.MailPath, u.Home, u.Driver}, "\x00")
}

func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserIndex {
	key := cacheKey(u)
	b.usersMu.Lock()
	ref, ok := b.users[key]
	if !ok {
		ui := &userIndex{
			b:           b,
			home:        u.Home,
			mailPath:    u.MailPath,
			driver:      u.Driver,
			separator:   mailbox.SepOrDefault(u.Separator),
			escapeChar:  u.StorageEscapeChar,
			volatileDir: u.VolatileDir,
			indexRoot:   u.IndexDir,
			username:    u.Username,
			owner:       makeOwner(u),
			open:        make(map[uint64]*folderState),
		}
		ref = &refUserIndex{ui: ui}
		b.users[key] = ref
	}
	ref.refs++
	b.usersMu.Unlock()
	// u.SessionID is the IMAP/POP3 login-proxy correlation ID (empty for LMTP
	// and other non-session contexts) — see folderState.traceID.
	return &userHandle{ui: ref.ui, b: b, cacheKey: key, traceID: u.SessionID}
}

// userHandle is the per-session view into a shared userIndex. It
// implements mailbox.UserIndex by forwarding all calls to the
// underlying userIndex. Close() decrements the reference count and
// evicts the entry when the last session disconnects.
type userHandle struct {
	ui       *userIndex
	b        *Backend
	cacheKey string
	// traceID is this session's correlation ID (see folderState.traceID).
	traceID string
}

// stampTrace records this handle's traceID on folderID's folderState (when
// open) before a call that will log against it, so the resulting log lines
// attribute to this session instead of whichever session opened the folder.
// No-op when traceID is empty (e.g. LMTP, or a test backend with no SessionID)
// or the folder isn't in the open cache (shouldn't happen — the caller already
// holds a live folderID).
func (h *userHandle) stampTrace(folderID uint64) {
	if h.traceID == "" {
		return
	}
	h.ui.mu.Lock()
	fs, ok := h.ui.open[folderID]
	h.ui.mu.Unlock()
	if ok {
		fs.mu.Lock()
		fs.traceID = h.traceID
		fs.mu.Unlock()
	}
}

func (h *userHandle) OpenFolder(folder string, uidValidity uint32) (*mailbox.Folder, error) {
	return h.ui.OpenFolder(folder, uidValidity, h.traceID)
}
func (h *userHandle) SaveFolder(f *mailbox.Folder) error { return h.ui.SaveFolder(f) }
func (h *userHandle) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	h.stampTrace(folderID)
	return h.ui.AppendMessage(folderID, m)
}
func (h *userHandle) AllocateUID(folderID uint64) (uint32, error) {
	h.stampTrace(folderID)
	return h.ui.AllocateUID(folderID)
}
func (h *userHandle) AllocateUIDWithModSeq(folderID uint64) (uint32, uint64, error) {
	h.stampTrace(folderID)
	return h.ui.AllocateUIDWithModSeq(folderID)
}
func (h *userHandle) AllocateAndAppend(folderID uint64, m *mailbox.MessageMeta) error {
	h.stampTrace(folderID)
	return h.ui.AllocateAndAppend(folderID, m)
}
func (h *userHandle) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return h.ui.UpdateFlags(folderID, uid, flags, keywords)
}

func (h *userHandle) UpdateFilename(folderID uint64, uid uint32, filename string) error {
	return h.ui.UpdateFilename(folderID, uid, filename)
}

func (h *userHandle) MarkFolderCorrupt(folderID uint64) error {
	return h.ui.MarkFolderCorrupt(folderID)
}

func (h *userHandle) ClearFolderCorrupt(folderID uint64) error {
	return h.ui.ClearFolderCorrupt(folderID)
}
func (h *userHandle) UpdateFlagsMulti(folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]uint64, error) {
	return h.ui.UpdateFlagsMulti(folderID, updates)
}
func (h *userHandle) SetAltTier(folderID uint64, filenames []string, altTier bool) error {
	return h.ui.SetAltTier(folderID, filenames, altTier)
}
func (h *userHandle) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return h.ui.GetMessages(folderID, uids)
}
func (h *userHandle) FolderVSize(folderID uint64) (uint64, uint32, error) {
	return h.ui.FolderVSize(folderID)
}
func (h *userHandle) RecomputeVSize(folderID uint64) error {
	return h.ui.RecomputeVSize(folderID)
}
func (h *userHandle) GUIDBackfillNeeded(folderID uint64) (bool, error) {
	return h.ui.GUIDBackfillNeeded(folderID)
}
func (h *userHandle) SetGUIDs(folderID uint64, guids map[uint32][16]byte) error {
	return h.ui.SetGUIDs(folderID, guids)
}
func (h *userHandle) ExpungeMessage(folderID uint64, uid uint32) error {
	return h.ui.ExpungeMessage(folderID, uid)
}
func (h *userHandle) NextModSeq(folderID uint64) (uint64, error) {
	return h.ui.NextModSeq(folderID)
}
func (h *userHandle) Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	return h.ui.Vanished(folderID, sinceModSeq)
}
func (h *userHandle) Keywords(folderID uint64) ([]string, error) {
	return h.ui.Keywords(folderID)
}
func (h *userHandle) RenameFolder(oldName, newName string) error {
	return h.ui.RenameFolder(oldName, newName)
}
func (h *userHandle) DeleteFolder(folder string) error {
	return h.ui.DeleteFolder(folder)
}
func (h *userHandle) GetPOP3UIDLs(folderID uint64) (map[uint32]string, error) {
	return h.ui.GetPOP3UIDLs(folderID)
}
func (h *userHandle) SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error {
	return h.ui.SavePOP3UIDLs(folderID, uidls)
}
func (h *userHandle) ResetFolder(folderID uint64, records []*mailbox.MessageMeta) ([]uint32, error) {
	return h.ui.ResetFolder(folderID, records)
}
func (h *userHandle) OptimizeIndex(folderID uint64) error {
	return h.ui.OptimizeIndex(folderID)
}

// Close decrements the session's reference to the shared userIndex.
// When the last session disconnects the underlying state is cleared
// and the entry is removed from the cache.
func (h *userHandle) Close() error {
	h.b.usersMu.Lock()
	ref := h.b.users[h.cacheKey]
	if ref != nil {
		ref.refs--
		if ref.refs <= 0 {
			delete(h.b.users, h.cacheKey)
			h.b.usersMu.Unlock()
			return h.ui.Close()
		}
	}
	h.b.usersMu.Unlock()
	return nil
}

// userIndex is the per-user, per-session UserIndex implementation.
// Each (user, folder) pair gets a folderState lazily on first
// OpenFolder call; subsequent OpenFolder reuses cached state.
type userIndex struct {
	b           *Backend
	home        string
	mailPath    string // mail root; index co-locates here when INDEX= is unset
	driver      string // mailbox driver; selects the per-folder layout
	separator   string // IMAP hierarchy separator; passed to FolderSubpath
	escapeChar  string // storage-name escape char; must match the mailbox driver or the trees diverge
	volatileDir string // base volatile dir (empty = disabled)
	indexRoot   string // INDEX= override root (empty = co-located with mail root)
	username    string
	owner       string

	mu    sync.Mutex
	next  uint64                  // monotonic per-session folder ID counter
	open  map[uint64]*folderState // folderID → state
	byDir map[string]uint64       // index dir path → folderID (dedup OpenFolder)
}

// folderState is the live in-memory snapshot of one folder's
// fileindex. Mutations append a transaction record to .index.log
// and update fs.file in-memory. The base .index file is only
// rewritten by flush() (OptimizeIndex / SaveFolder / ResetFolder /
// createFresh).
//
// logSize is the log file byte count after the last write or reload;
// reload() compares it against a fresh stat so it can skip the
// expensive mailindex.Open when nothing has changed (common case
// within a single pod). baseMod is the base .index mtime at the last
// full reload — it changes only when another writer calls flush().
type folderState struct {
	mu sync.RWMutex

	folder      string // mailbox folder name (e.g. "INBOX", "Sent")
	indexDir    string // <home>/<folder-relative>/
	indexPath   string // <indexDir>/yarilo.index
	volatileDir string // local dir for tmp files (empty = same as indexDir)

	file      *mailindex.File // the wire-format snapshot
	keywords  keywordsHdr     // parsed keyword name registry
	filenames map[uint32]string
	sizes     map[uint32]uint32 // UID → message size in bytes, stored in .names sidecar

	logSize int64     // byte count of .index.log after last write/reload
	baseMod time.Time // mtime of base .index at last full reload
	// baseIdent is the os.FileInfo captured with baseMod, compared via
	// os.SameFile (inode+device) not mtime alone: coarse mtime resolution can
	// hide a same-tick replace of the base .index by a concurrent flush()/
	// Recreate(), which would let reload()'s fast path trust a stale snapshot.
	baseIdent os.FileInfo
	lastFlush time.Time // wall-clock time of last flush() call (zero = never)

	// logFD and namesFD are kept open across calls so appendMutLog /
	// appendName each cost one write(2) instead of open+stat+write+close.
	// Callers must invoke closeFDs() before any operation that replaces
	// these files on disk (truncateLog, saveNames).
	logFD   *os.File
	namesFD *os.File

	// dboxHdr is the folder GUID + flags from the dbox-hdr ext.
	hdr dboxHdr

	// vsize caches the aggregate virtual size from the hdr-vsize ext. Kept in
	// step with appends/expunges and persisted back to the ext header on flush;
	// self-heals via recalcVsizeLocked when {HighestUID, MessageCount} drift
	// from the folder state (e.g. after a legacy import or crash).
	vsize hdrVsize

	// traceID is the calling session's correlation ID (empty for LMTP),
	// refreshed by every userHandle call touching this folder. Since userIndex
	// is shared across a user's sessions, it tracks whichever session most
	// recently touched the folder, enough to grep one session's log lines.
	traceID string
}

// closeFDs closes logFD and namesFD and sets them to nil.
// Must be called before truncateLog or saveNames replaces the files on disk,
// and when the folderState is evicted from the open-folder cache.
func (fs *folderState) closeFDs() {
	if fs.logFD != nil {
		_ = fs.logFD.Close()
		fs.logFD = nil
	}
	if fs.namesFD != nil {
		_ = fs.namesFD.Close()
		fs.namesFD = nil
	}
}

// compactLogIfNeeded checks whether fs.logSize has crossed the compaction
// thresholds configured on the Backend and, if so, flushes the base index
// and resets the log. Errors are non-fatal — the log simply stays larger
// until the next successful compaction attempt.
//
// Rotation logic:
//
//	rotate if logSize > maxBytes
//	     OR (logSize >= minBytes AND age since lastFlush >= minAge)
//
// Caller must hold fs.mu.
func (u *userIndex) compactLogIfNeeded(fs *folderState) {
	min := u.b.logCompactMinBytes
	if min == 0 {
		return // compaction disabled
	}
	max := u.b.logCompactMaxBytes
	age := u.b.logCompactMinAge

	needCompact := fs.logSize > max ||
		(fs.logSize >= min && (fs.lastFlush.IsZero() || time.Since(fs.lastFlush) >= age))
	if !needCompact {
		return
	}
	// Never flush our in-memory header if the shared log was replaced by another
	// process's compaction since our last reload: that header could carry a
	// stale (lower) NextUID and regress the folder's UID counter. Under the
	// distributed lock this cannot happen; if it ever does, bail and let the
	// next reload reconcile from the rewritten base.
	if fs.logFileReplaced() {
		slog.Warn("fileindex: skipping compaction, .log replaced since reload", "folder", fs.folder)
		fs.closeFDs()
		return
	}
	if err := fs.flush(false); err != nil {
		slog.Warn("fileindex: log compaction flush failed", "folder", fs.folder, "err", err)
		return
	}
	fs.closeFDs()
	if err := truncateLog(fs.indexPath, fs.file.Header.IndexID); err != nil {
		slog.Warn("fileindex: log compaction truncate failed", "folder", fs.folder, "err", err)
		return
	}
	fs.logSize = 0
}

// fdMatchesFile reports whether f still refers to the same on-disk file
// (device + inode, via os.SameFile) as fi. Returns false when f is nil or
// either stat is unavailable — an unprovable identity is treated as "not the
// same file". Single source of the inode-identity comparison used by reload()
// and logFileReplaced().
func fdMatchesFile(f *os.File, fi os.FileInfo) bool {
	if f == nil || fi == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil || st == nil {
		return false
	}
	return os.SameFile(fi, st)
}

// logFileReplaced reports whether the on-disk .log is a different file
// (inode+device) than the one fs.logFD currently holds open — i.e. another
// process replaced it through truncateLog's rename. Returns false when we hold
// no fd yet or the path stat fails (treat as "not proven replaced"). Caller
// must hold fs.mu.
func (fs *folderState) logFileReplaced() bool {
	if fs.logFD == nil {
		return false
	}
	logStat, err := os.Stat(fs.indexPath + ".log")
	if err != nil || logStat == nil {
		return false
	}
	return !fdMatchesFile(fs.logFD, logStat)
}

// makeOwner builds the owner string passed to yarilo-locks BUSY
// reports. Format mirrors the maildir / dbox / mdbox backends.
func makeOwner(u *mailbox.UserInfo) string {
	proc := "yarilo"
	if len(os.Args) > 0 {
		proc = filepath.Base(os.Args[0])
	}
	return fmt.Sprintf("%s/%d/%s", proc, os.Getpid(), u.Username)
}

// indexRootDir resolves the index root: INDEX= (indexRoot) when set,
// otherwise the mail root (mailPath), falling back to Home. Mirrors
// resolves to the index root (INDEX=), else the mail root, else Home.
func (u *userIndex) indexRootDir() string {
	root := u.home
	if u.mailPath != "" {
		root = u.mailPath
	}
	if u.indexRoot != "" {
		root = u.indexRoot
	}
	return root
}

// indexDir is the per-folder index directory: the index root joined with the
// driver's folder sub-layout, so the index mirrors the mailbox tree.
func (u *userIndex) indexDir(folder string) string {
	return filepath.Join(u.indexRootDir(), mailbox.FolderSubpathEscaped(u.driver, folder, folder, u.separator, u.escapeChar))
}

// folderVolatileDir returns the per-folder volatile directory when
// volatileDir is configured, or "" when disabled.
func (u *userIndex) folderVolatileDir(folder string) string {
	if u.volatileDir == "" {
		return ""
	}
	return filepath.Join(u.volatileDir, mailbox.FolderSubpathEscaped(u.driver, folder, folder, u.separator, u.escapeChar))
}

// withFolderRO reloads the folder state, then runs read-only fn against the
// settled in-memory snapshot.
//
// The reload takes a SHARED distributed lock so it cannot interleave with
// another process's lock-holding compaction (flush + truncateLog) and load a
// torn view into the shared folderState — which every later locked write would
// then trust as a baseline, regressing NextUID. Shared holders run
// concurrently and only block against an in-flight exclusive writer; fn then
// reads under fs.mu.RLock without holding the distributed lock at all.
func (u *userIndex) withFolderRO(folderID uint64, fn func(*folderState) error) error {
	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	err := u.withDistLock(fs, true, func() error {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.reload()
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// fn only reads the in-memory snapshot; shared lock allows
	// concurrent readers without blocking writers.
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fn(fs)
}

// withFolderLock runs fn under the cross-process index lock for
// the supplied folder state. When no locker is wired (tests),
// fn runs unguarded.
//
// The HoldsResource() shortcut is preserved here so the POP3 QUIT
// pattern (outer caller takes the lock then drives per-message
// storage calls that touch the same key) does not deadlock against
// itself.
func (u *userIndex) withFolderLock(fs *folderState, fn func() error) error {
	return u.withDistLock(fs, false, func() error {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		t1 := time.Now()
		err := fn()
		slog.Debug("fileindex: lock fn",
			"user", u.username, "folder", fs.folder,
			"fn_ms", time.Since(t1).Milliseconds())
		return err
	})
}

// withDistLock runs fn while holding the cross-process index lock for fs.folder.
// It acquires the lock BEFORE fn touches fs.mu so a slow lock-wait (up to 35 s)
// does not block concurrent readers that only need fs.mu.RLock(). The
// HoldsResource() shortcut keeps it re-entrant: an outer caller that already
// holds the key (the POP3 QUIT pattern, or withFolderRO nested inside a locked
// write) runs fn without re-acquiring, so it cannot deadlock against itself.
// When no locker is wired (tests) fn runs unguarded.
//
// shared selects a shared (read) lock instead of the default exclusive one:
// multiple shared holders run concurrently, blocking only against an in-flight
// exclusive writer.
func (u *userIndex) withDistLock(fs *folderState, shared bool, fn func() error) error {
	if u.b.locker != nil {
		key := locks.MailboxKey(u.username, fs.folder)
		if !u.b.locker.HoldsResource(key) {
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			t0 := time.Now()
			var lk locks.Lock
			var err error
			if shared {
				lk, err = locks.AcquireShared(ctx, u.b.locker, key, u.owner, 30*time.Second)
			} else {
				lk, err = locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
			}
			if err != nil {
				return fmt.Errorf("fileindex/lock %s: %w", fs.folder, err)
			}
			slog.Debug("fileindex: lock wait",
				"user", u.username, "folder", fs.folder, "shared", shared,
				"lock_wait_ms", time.Since(t0).Milliseconds())
			defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
		}
	}
	return fn()
}

// withTwoFolderLocks acquires the X locks for folderA and folderB
// in lexicographic order. Used by RenameFolder so the two-folder
// invariant cannot deadlock against another rename or against a
// driver-level multi-folder operation.
func (u *userIndex) withTwoFolderLocks(folderA, folderB string, fn func() error) error {
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
			return fmt.Errorf("fileindex/lock %s: %w", a, err)
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
			return fmt.Errorf("fileindex/lock %s: %w", b, err)
		}
		defer func() { _ = u.b.locker.Unlock(ctx, lkB.ID) }()
	}
	return fn()
}

// Close releases every cached folder state. The fileindex itself
func (u *userIndex) Close() error {
	u.mu.Lock()
	for _, fs := range u.open {
		fs.closeFDs()
	}
	u.open = nil
	u.byDir = nil
	u.mu.Unlock()
	return nil
}

// RenameFolder moves the on-disk index directory from oldName to
// newName. Both per-folder locks are taken in lexicographic
// order so concurrent renames + writes do not deadlock.
func (u *userIndex) RenameFolder(oldName, newName string) error {
	return u.withTwoFolderLocks(oldName, newName, func() error {
		oldDir := u.folderTreeDir(u.indexDir(oldName))
		newDir := u.folderTreeDir(u.indexDir(newName))
		if _, err := os.Stat(oldDir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
			return fmt.Errorf("fileindex/rename: mkdir parent: %w", err)
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			return fmt.Errorf("fileindex/rename %s → %s: %w", oldDir, newDir, err)
		}
		// Invalidate any cached folderState pointing at the old path.
		u.mu.Lock()
		for id, fs := range u.open {
			if fs.folder == oldName {
				fs.closeFDs()
				delete(u.open, id)
			}
		}
		if u.byDir != nil {
			delete(u.byDir, u.indexDir(oldName))
		}
		u.mu.Unlock()
		return nil
	})
}

// folderTreeDir is the index subtree owned solely by folder. For dbox
// drivers indexDir() points at the dbox-Mails leaf, so its parent is the
// folder dir; other drivers already return the folder dir. Delete/Rename
// operate on this so the whole subtree is reclaimed/moved, leaving no
// empty mailboxes/<name> shell behind.
func (u *userIndex) folderTreeDir(dir string) string {
	switch u.driver {
	case "mdbox", "sdbox", "dbox":
		return filepath.Dir(dir)
	}
	return dir
}

// DeleteFolder removes the on-disk index directory (and its volatile
// twin) for folder. Called by IMAP DELETE after UserMailbox.Delete
// succeeds so the index does not outlive the mailbox it describes.
// Idempotent: a missing directory is not an error.
func (u *userIndex) DeleteFolder(folder string) error {
	return u.withTwoFolderLocks(folder, folder, func() error {
		dir := u.folderTreeDir(u.indexDir(folder))
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("fileindex/delete %s: %w", dir, err)
		}
		if vd := u.folderVolatileDir(folder); vd != "" {
			if err := os.RemoveAll(u.folderTreeDir(vd)); err != nil {
				return fmt.Errorf("fileindex/delete volatile %s: %w", vd, err)
			}
		}
		u.mu.Lock()
		for id, fs := range u.open {
			if fs.folder == folder {
				fs.closeFDs()
				delete(u.open, id)
			}
		}
		if u.byDir != nil {
			delete(u.byDir, u.indexDir(folder))
		}
		u.mu.Unlock()
		return nil
	})
}

// ---- internal helpers shared across folder.go + legacy.go ----

// flag bits — mirror mailindex.MailFlag values but kept as their
// own constants for grep-ability and to decouple from a future
// mailindex API rename.
const (
	flagAnswered = uint8(mailindex.FlagAnswered)
	flagFlagged  = uint8(mailindex.FlagFlagged)
	flagDeleted  = uint8(mailindex.FlagDeleted)
	flagSeen     = uint8(mailindex.FlagSeen)
	flagDraft    = uint8(mailindex.FlagDraft)
)

// imapFlagsToIndex converts an IMAP flag list to the per-record
// flag byte used in mail-index records.
func imapFlagsToIndex(flags []string) uint8 {
	var b uint8
	for _, f := range flags {
		switch f {
		case `\Answered`:
			b |= flagAnswered
		case `\Flagged`:
			b |= flagFlagged
		case `\Deleted`:
			b |= flagDeleted
		case `\Seen`:
			b |= flagSeen
		case `\Draft`:
			b |= flagDraft
		}
	}
	return b
}

// indexFlagsToIMAP is the inverse: per-record flag byte → IMAP
// flag list (stable order, useful for tests).
func indexFlagsToIMAP(b uint8) []string {
	var flags []string
	if b&flagAnswered != 0 {
		flags = append(flags, `\Answered`)
	}
	if b&flagFlagged != 0 {
		flags = append(flags, `\Flagged`)
	}
	if b&flagDeleted != 0 {
		flags = append(flags, `\Deleted`)
	}
	if b&flagSeen != 0 {
		flags = append(flags, `\Seen`)
	}
	if b&flagDraft != 0 {
		flags = append(flags, `\Draft`)
	}
	return flags
}

// seqSetContains reports whether uid falls in the supplied set.
// An empty SeqSet matches everything (the common GetMessages
// "give me all records" idiom).
func seqSetContains(s mailbox.SeqSet, uid uint32) bool {
	if len(s) == 0 {
		return true
	}
	for _, r := range s {
		if r.From == 0 && r.To == 0 {
			return true
		}
		hi := r.To
		if hi == 0 {
			hi = ^uint32(0)
		}
		if uid >= r.From && uid <= hi {
			return true
		}
	}
	return false
}

// generateGUID returns a fresh random 16-byte folder GUID.
// Used on first OpenFolder when no on-disk dbox-hdr exists.
func generateGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}

// namesPath is the .names sidecar path for an index directory.
// On-disk filenames. yarilo writes under the yarilo-native names;
// legacy canonical names are read once at OpenFolder time and
// renamed in place so subsequent runs see only yarilo files.
const (
	IndexFileName            = "yarilo.index"
	IndexLogFileName         = "yarilo.index.log"
	IndexNamesFileName       = "yarilo.index.names"
	LegacyIndexFileName      = "dovecot.index"
	LegacyIndexLogFileName   = "dovecot.index.log"
	LegacyIndexNamesFileName = "dovecot.index.names"
)

func indexPathFor(indexDir string) string { return filepath.Join(indexDir, IndexFileName) }
func namesPath(indexDir string) string    { return filepath.Join(indexDir, IndexNamesFileName) }

// migrateLegacyFilenames promotes legacy canonical filenames in
// indexDir to their yarilo-native equivalents. Atomic per-file
// rename. Idempotent: skips files whose yarilo-native counterpart
// already exists (the operator may have run a previous migration).
// Returns an error only on a partial rename — never on absence of
// the legacy file.
func migrateLegacyFilenames(indexDir string) error {
	pairs := []struct{ legacy, native string }{
		{LegacyIndexFileName, IndexFileName},
		{LegacyIndexLogFileName, IndexLogFileName},
		{LegacyIndexNamesFileName, IndexNamesFileName},
	}
	for _, p := range pairs {
		legacyPath := filepath.Join(indexDir, p.legacy)
		nativePath := filepath.Join(indexDir, p.native)
		if _, err := os.Stat(nativePath); err == nil {
			continue
		}
		if _, err := os.Stat(legacyPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("fileindex/migrate: stat %s: %w", legacyPath, err)
		}
		if err := os.Rename(legacyPath, nativePath); err != nil {
			// A concurrent opener may have renamed this pair between the stats
			// above and here; os.Rename is atomic, so the loser sees ENOENT.
			// Re-check nativePath: if it now exists the migration is already
			// done and there is nothing left to do.
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Stat(nativePath); statErr == nil {
					continue
				}
			}
			return fmt.Errorf("fileindex/migrate: rename %s → %s: %w", legacyPath, nativePath, err)
		}
	}
	return nil
}

// loadNames reads the .names sidecar into UID→filename and
// UID→size maps. Missing file = empty maps (not an error).
// Format is TSV with 2 or 3 columns:
//
//	<uid>\t<filename>\n               (legacy — size treated as 0)
//	<uid>\t<filename>\t<size_bytes>\n (current)
func loadNames(indexDir string) (map[uint32]string, map[uint32]uint32) {
	names := map[uint32]string{}
	sizes := map[uint32]uint32{}
	f, err := os.Open(namesPath(indexDir))
	if err != nil {
		return names, sizes
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		tab1 := strings.IndexByte(line, '\t')
		if tab1 < 0 {
			continue
		}
		uid64, err := strconv.ParseUint(line[:tab1], 10, 32)
		if err != nil {
			continue
		}
		uid := uint32(uid64)
		rest := line[tab1+1:]
		if tab2 := strings.IndexByte(rest, '\t'); tab2 >= 0 {
			names[uid] = rest[:tab2]
			if sz, err := strconv.ParseUint(rest[tab2+1:], 10, 32); err == nil {
				sizes[uid] = uint32(sz)
			}
		} else {
			names[uid] = rest
		}
	}
	return names, sizes
}

// appendName appends a single uid→filename→size entry to the .names sidecar.
// fs.namesFD is kept open across calls so each append costs one write(2).
// closeFDs() must be called before saveNames replaces the file on disk.
// loadNames handles duplicate UIDs (last entry wins), so this is safe.
func (fs *folderState) appendName(uid uint32, filename string, size uint32) error {
	if fs.namesFD == nil {
		dst := namesPath(fs.indexDir)
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("fileindex/names: open: %w", err)
		}
		fs.namesFD = f
	}
	if _, err := fmt.Fprintf(fs.namesFD, "%d\t%s\t%d\n", uid, filename, size); err != nil {
		_ = fs.namesFD.Close()
		fs.namesFD = nil
		return fmt.Errorf("fileindex/names: write: %w", err)
	}
	return nil
}

// saveNames rewrites the .names sidecar atomically (.tmp + rename). Each
// line: <uid>\t<filename>\t<size_bytes>\n. volatileDir routes the scratch
// write to the fast local volume when configured, matching every other
// sidecar/base rewrite in this package.
func saveNames(indexDir, volatileDir string, names map[uint32]string, sizes map[uint32]uint32) error {
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return fmt.Errorf("fileindex/names: mkdir: %w", err)
	}
	if volatileDir != "" {
		if err := os.MkdirAll(volatileDir, 0o700); err != nil {
			return fmt.Errorf("fileindex/names: mkdir volatile: %w", err)
		}
	}
	dst := namesPath(indexDir)
	tmp := sidecarTmpPath(dst, volatileDir)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/names: open tmp: %w", err)
	}
	// Collect all UIDs from both maps so messages with no filename
	// (but a known size) are still persisted.
	all := make(map[uint32]struct{}, len(names))
	for uid := range names {
		all[uid] = struct{}{}
	}
	for uid := range sizes {
		all[uid] = struct{}{}
	}
	bw := bufio.NewWriter(f)
	for uid := range all {
		fmt.Fprintf(bw, "%d\t%s\t%d\n", uid, names[uid], sizes[uid]) //nolint:errcheck
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/names: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/names: close: %w", err)
	}
	if err := sidecarCommit(tmp, dst, volatileDir); err != nil {
		return fmt.Errorf("fileindex/names: rename: %w", err)
	}
	return nil
}

// ensureLogStub writes an empty .log file (just LogHeader) if
// none exists. Required because the canonical reader fails
// hard when .index is present but its .log sibling is missing.
// Pure-Recreate mode never appends to the log, but the file
// must exist for compat. volatileDir routes the scratch write to the fast
// local volume when configured, matching every other sidecar/base rewrite.
func ensureLogStub(indexPath, volatileDir string, indexID uint32) error {
	logPath := indexPath + ".log"
	if _, err := os.Stat(logPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileindex/log stub: stat: %w", err)
	}
	if volatileDir != "" {
		if err := os.MkdirAll(volatileDir, 0o700); err != nil {
			return fmt.Errorf("fileindex/log stub: mkdir volatile: %w", err)
		}
	}
	tmp := sidecarTmpPath(logPath, volatileDir)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/log stub: create: %w", err)
	}
	hdr := mailindex.NewLogHeader(indexID, 1, uint32(time.Now().Unix()))
	if err := hdr.Encode(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log stub: encode header: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log stub: close: %w", err)
	}
	if err := sidecarCommit(tmp, logPath, volatileDir); err != nil {
		return fmt.Errorf("fileindex/log stub: rename: %w", err)
	}
	return nil
}

// debugLog wraps slog.Debug calls so the package can quiet down
// in tests by setting slog default's level to LevelInfo. Kept as
// a free function so it has no method-receiver allocation.
func debugLog(msg string, kv ...any) { slog.Debug("fileindex: "+msg, kv...) }

// Cache-pair surface (#1030), forwarded like every other folder verb so the
// interface assertion in the IMAP layer sees it on the handle sessions hold.
func (h *userHandle) CachePairIdentity(folderID uint64) (uint32, uint32, bool, error) {
	h.stampTrace(folderID)
	return h.ui.CachePairIdentity(folderID)
}

func (h *userHandle) CachePath(folderID uint64) (string, error) {
	return h.ui.CachePath(folderID)
}

func (h *userHandle) SetCacheOffsets(folderID uint64, offsets map[uint32]uint32) error {
	h.stampTrace(folderID)
	return h.ui.SetCacheOffsets(folderID, offsets)
}

// PurgeCache forwards the cache purge (#1030) like every other folder verb.
func (h *userHandle) PurgeCache(folderID uint64) (int, int64, error) {
	h.stampTrace(folderID)
	return h.ui.PurgeCache(folderID)
}

// EnsureCacheExtension forwards the lazy add (#1184).
func (h *userHandle) EnsureCacheExtension(folderID uint64) (uint32, uint32, error) {
	h.stampTrace(folderID)
	return h.ui.EnsureCacheExtension(folderID)
}

// BumpCacheGeneration forwards the generation bump (#1184).
func (h *userHandle) BumpCacheGeneration(folderID uint64) (uint32, error) {
	h.stampTrace(folderID)
	return h.ui.BumpCacheGeneration(folderID)
}
