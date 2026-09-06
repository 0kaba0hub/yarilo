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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/logrotate"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/internal/userstate/folders"
	"github.com/yarilomail/yarilo/internal/userstate/uidvalidity"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The fold thresholds live in one place for both logs; see internal/storage/logrotate.
var (
	defaultLogCompactMinBytes = logrotate.MinSize
	defaultLogCompactMaxBytes = logrotate.MaxSize
	defaultLogCompactMinAge   = logrotate.MinAge
)

// sidecarTmpSeq gives concurrent sidecar writers unique tmp names, so a shared
// "<path>.tmp" cannot make the loser's rename fail after the winner consumes it.
var sidecarTmpSeq atomic.Uint64

// sidecarTmpPath is a unique working path for a sidecar rewrite of dst. With
// volatileDir the caller must stage it back: os.Rename cannot cross devices.
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

// sidecarCommit renames tmp onto dst, staging through a same-device copy when
// volatileDir put tmp on another filesystem. Cleans up its own scratch files.
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

// Backend is the per-process factory for fileindex handles. It holds only
// process-wide state; per-user state lives in userIndex (created by OpenUser).
type Backend struct {
	locker locks.Locker

	// listUTF8 is the on-disk folder name encoding, mirroring the mailbox
	// backends' option of the same name. Default true, as theirs is.
	listUTF8 bool

	// noCreate fails OpenFolder instead of initialising a missing index: a
	// fabricated one reads as healthy and empty, hiding what the store held.
	noCreate bool

	logCompactMinBytes int64
	logCompactMaxBytes int64
	logCompactMinAge   time.Duration

	// users caches one userIndex per username, so a pod's sessions serialise on
	// fs.mu rather than contending the cross-process lock.
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

// WithLocker wires a lock client in, so every mutation takes the cross-process
// X lock. Nil disables that -- for unit tests, never safe in production.
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// WithListUTF8 sets the on-disk name encoding; it must match the mailbox
// backend's, or the two trees spell a folder differently (#1586).
func WithListUTF8(v bool) Option { return func(b *Backend) { b.listUTF8 = v } }

// WithNoCreate refuses to initialise a missing folder index. Use it in any tool
// that must observe a store rather than establish one.
func WithNoCreate() Option {
	return func(b *Backend) { b.noCreate = true }
}

// WithLogCompaction sets the compaction thresholds; a zero keeps that arm's
// default, so setting one leaves the others alone. No way to disable rotation.
func WithLogCompaction(minBytes, maxBytes int64, minAge time.Duration) Option {
	return func(b *Backend) {
		if minBytes != 0 {
			b.logCompactMinBytes = minBytes
		}
		if maxBytes != 0 {
			b.logCompactMaxBytes = maxBytes
		}
		if minAge != 0 {
			b.logCompactMinAge = minAge
		}
	}
}

// New constructs a Backend.
func New(opts ...Option) *Backend {
	b := &Backend{
		listUTF8:           true,
		users:              make(map[string]*refUserIndex),
		logCompactMinBytes: defaultLogCompactMinBytes,
		logCompactMaxBytes: defaultLogCompactMaxBytes,
		logCompactMinAge:   defaultLogCompactMinAge,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u. All sessions for the same
// username share one userIndex, serialising on the in-process mutex rather
// than competing for the cross-process lock.
// cacheKey identifies a shared userIndex: username plus every field that
// determines the on-disk index root, so distinct storage roots for one user
// get separate index state instead of colliding.
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
			listUTF8:    b.listUTF8,
			volatileDir: u.VolatileDir,
			indexRoot:   u.IndexDir,
			// Through the same function every other service calls: a second
			// spelling of a path rule is what misplaced our map (#1579).
			controlRoot: mailbox.ControlRoot(u),
			username:    u.Username,
			// The shared index keeps one for the paths that have no handle in hand;
			// a locked call made through a handle stamps its own (#1664).
			owner: locks.Owner(u.Username, u.LockID()),
			open:  make(map[uint64]*folderState),
		}
		ui.uidValidity = uidvalidity.New(ui.controlRoot, ui.username, ui.owner, b.locker)
		ui.folders = folders.New(ui.controlRoot, ui.username, ui.owner, b.locker)
		ref = &refUserIndex{ui: ui}
		b.users[key] = ref
	}
	ref.refs++
	b.usersMu.Unlock()
	// u.SessionID is the IMAP/POP3 login-proxy correlation ID (empty for LMTP
	// and other non-session contexts) — see folderState.traceID.
	return &userHandle{
		ui: ref.ui, b: b, cacheKey: key,
		traceID: u.SessionID,
		owner:   locks.Owner(u.Username, u.LockID()),
	}
}

// userHandle is the per-session view into a shared userIndex, forwarding every
// call to it. Close() decrements the refcount and evicts on the last session.
type userHandle struct {
	ui       *userIndex
	b        *Backend
	cacheKey string
	// owner is this session's own; the shared userIndex keeps its creator's,
	// which is the name held_by used to report for everyone else (#1664).
	owner string
	// traceID is this session's correlation ID (see folderState.traceID).
	traceID string
}

// lockOwner is what this folder's next acquisition announces: the session that
// touched it last, or the shared index's for a path with no handle.
func (fs *folderState) lockOwner(fallback string) string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.owner != "" {
		return fs.owner
	}
	return fallback
}

// stamped records this session's trace id and lock owner on the folder state and
// returns the index: every folder verb goes through it, so none can announce the
// name of whichever session opened the folder (#1664).
func (h *userHandle) stamped(folderID uint64) *userIndex {
	if h.traceID == "" && h.owner == "" {
		return h.ui
	}
	h.ui.mu.Lock()
	fs, ok := h.ui.open[folderID]
	h.ui.mu.Unlock()
	if ok {
		fs.mu.Lock()
		if h.traceID != "" {
			fs.traceID = h.traceID
		}
		if h.owner != "" {
			fs.owner = h.owner
		}
		fs.mu.Unlock()
	}
	return h.ui
}

func (h *userHandle) OpenFolder(folder string, uidValidity uint32) (*mailbox.Folder, error) {
	return h.ui.OpenFolder(folder, uidValidity, h.traceID)
}

// CreateFolder satisfies mailbox.FolderCreator. On the handle as well as on the
// index, because callers hold the handle.
func (h *userHandle) CreateFolder(folder string, uidValidity uint32) (*mailbox.Folder, error) {
	return h.ui.CreateFolder(folder, uidValidity, h.traceID)
}
func (h *userHandle) SaveFolder(f *mailbox.Folder) error { return h.ui.SaveFolder(f) }

// AdoptUIDSpace satisfies mailbox.UIDSpaceAdopter, on the handle as well as the
// index: a capability only on the inner type is one no assertion finds.
func (h *userHandle) AdoptUIDSpace(folderID uint64, uidValidity, nextUID uint32) error {
	return h.stamped(folderID).AdoptUIDSpace(folderID, uidValidity, nextUID)
}

func (h *userHandle) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	return h.stamped(folderID).AppendMessage(folderID, m)
}
func (h *userHandle) AllocateUID(folderID uint64) (uint32, error) {
	return h.stamped(folderID).AllocateUID(folderID)
}
func (h *userHandle) AllocateUIDWithModSeq(folderID uint64) (uint32, uint64, error) {
	return h.stamped(folderID).AllocateUIDWithModSeq(folderID)
}
func (h *userHandle) AllocateAndAppend(folderID uint64, m *mailbox.MessageMeta) error {
	return h.stamped(folderID).AllocateAndAppend(folderID, m)
}
func (h *userHandle) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return h.stamped(folderID).UpdateFlags(folderID, uid, flags, keywords)
}

func (h *userHandle) AddFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return h.stamped(folderID).AddFlags(folderID, uid, flags, keywords)
}

func (h *userHandle) RemoveFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return h.stamped(folderID).RemoveFlags(folderID, uid, flags, keywords)
}

func (h *userHandle) UpdateFilename(folderID uint64, uid uint32, filename string) error {
	return h.stamped(folderID).UpdateFilename(folderID, uid, filename)
}

// UpdateFilenames satisfies mailbox.FilenameWriterMulti. On the handle too:
// callers hold it, and a method only on *userIndex fails the assertion silently.
func (h *userHandle) UpdateFilenames(folderID uint64, names map[uint32]string) error {
	return h.stamped(folderID).UpdateFilenames(folderID, names)
}

func (h *userHandle) MarkFolderCorrupt(folderID uint64) error {
	return h.stamped(folderID).MarkFolderCorrupt(folderID)
}

func (h *userHandle) ClearFolderCorrupt(folderID uint64) error {
	return h.stamped(folderID).ClearFolderCorrupt(folderID)
}
func (h *userHandle) UpdateFlagsMulti(folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsResult, error) {
	return h.stamped(folderID).UpdateFlagsMulti(folderID, updates)
}
func (h *userHandle) SetAltTier(folderID uint64, filenames []string, altTier bool) error {
	return h.stamped(folderID).SetAltTier(folderID, filenames, altTier)
}
func (h *userHandle) GetPOP3UIDLsUnlocked(folderID uint64) (map[uint32]string, error) {
	return h.stamped(folderID).GetPOP3UIDLsUnlocked(folderID)
}

func (h *userHandle) VanishedUnlocked(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	return h.stamped(folderID).VanishedUnlocked(folderID, sinceModSeq)
}

func (h *userHandle) KeywordsUnlocked(folderID uint64) ([]string, error) {
	return h.stamped(folderID).KeywordsUnlocked(folderID)
}

func (h *userHandle) GetMessagesUnlocked(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return h.stamped(folderID).GetMessagesUnlocked(folderID, uids)
}

func (h *userHandle) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return h.stamped(folderID).GetMessages(folderID, uids)
}
func (h *userHandle) FolderVSize(folderID uint64) (uint64, uint32, error) {
	return h.stamped(folderID).FolderVSize(folderID)
}
func (h *userHandle) RecomputeVSize(folderID uint64) error {
	return h.stamped(folderID).RecomputeVSize(folderID)
}
func (h *userHandle) GUIDBackfillNeeded(folderID uint64) (bool, error) {
	return h.stamped(folderID).GUIDBackfillNeeded(folderID)
}
func (h *userHandle) SetGUIDs(folderID uint64, guids map[uint32][16]byte) error {
	return h.stamped(folderID).SetGUIDs(folderID, guids)
}
func (h *userHandle) ExpungeMessage(folderID uint64, uid uint32) error {
	return h.stamped(folderID).ExpungeMessage(folderID, uid)
}
func (h *userHandle) NextModSeq(folderID uint64) (uint64, error) {
	return h.stamped(folderID).NextModSeq(folderID)
}
func (h *userHandle) Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	return h.stamped(folderID).Vanished(folderID, sinceModSeq)
}
func (h *userHandle) VanishedGUIDs(folderID uint64, sinceModSeq uint64) ([][16]byte, bool, error) {
	return h.stamped(folderID).VanishedGUIDs(folderID, sinceModSeq)
}
func (h *userHandle) FolderStamp(folder string) (mailbox.FolderStamp, error) {
	return h.ui.FolderStamp(folder)
}
func (h *userHandle) ExpungeFloor(folderID uint64) (uint64, error) {
	return h.stamped(folderID).ExpungeFloor(folderID)
}
func (h *userHandle) Keywords(folderID uint64) ([]string, error) {
	return h.stamped(folderID).Keywords(folderID)
}
func (h *userHandle) RenameFolder(oldName, newName string) error {
	return h.ui.RenameFolder(oldName, newName)
}
func (h *userHandle) DeleteFolder(folder string) error {
	return h.ui.DeleteFolder(folder)
}
func (h *userHandle) GetPOP3UIDLs(folderID uint64) (map[uint32]string, error) {
	return h.stamped(folderID).GetPOP3UIDLs(folderID)
}
func (h *userHandle) SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error {
	return h.stamped(folderID).SavePOP3UIDLs(folderID, uidls)
}
func (h *userHandle) ResetFolder(folderID uint64, records []*mailbox.MessageMeta) ([]uint32, error) {
	return h.stamped(folderID).ResetFolder(folderID, records)
}
func (h *userHandle) OptimizeIndex(folderID uint64) error {
	return h.stamped(folderID).OptimizeIndex(folderID)
}
func (h *userHandle) JournalSizes(folderID uint64) (int64, int64, error) {
	return h.stamped(folderID).JournalSizes(folderID)
}

// Close decrements the session's reference; the last one clears the state and
// removes the cache entry.
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

// userIndex is the per-user UserIndex implementation; each (user, folder) pair
// gets a folderState lazily on first OpenFolder and reuses it after.
type userIndex struct {
	b           *Backend
	home        string
	mailPath    string // mail root; index co-locates here when INDEX= is unset
	driver      string // mailbox driver; selects the per-folder layout
	separator   string // IMAP hierarchy separator; passed to FolderSubpath
	escapeChar  string // storage-name escape char; must match the mailbox driver or the trees diverge
	listUTF8    bool   // on-disk folder name encoding; must match the mailbox driver for the same reason
	volatileDir string // base volatile dir (empty = disabled)
	indexRoot   string // INDEX= override root (empty = co-located with mail root)
	controlRoot string // where per-user control state lives (subscriptions)
	// uidValidity hands out a value for a folder that has no past, and never
	// hands one out twice.
	uidValidity *uidvalidity.Allocator
	// folders remembers what each folder was created with, so losing a folder
	// index does not lose its identity.
	folders  *folders.Store
	username string
	owner    string

	mu    sync.Mutex
	next  uint64                  // monotonic per-session folder ID counter
	open  map[uint64]*folderState // folderID → state
	byDir map[string]uint64       // index dir path → folderID (dedup OpenFolder)
}

// folderState is one folder's fileindex in memory: mutations append to the log
// and update fs.file, and only flush() rewrites the base.
type folderState struct {
	mu sync.RWMutex

	folder      string // mailbox folder name (e.g. "INBOX", "Sent")
	indexDir    string // <home>/<folder-relative>/
	indexPath   string // <indexDir>/yarilo.index
	volatileDir string // local dir for tmp files (empty = same as indexDir)

	// intent is why this folder is being opened. A missing index means
	// different things to the two answers.
	intent openIntent

	// fsyncOnFlush makes the next base rewrite durable before its rename, set
	// only where the only other copy is removed right after (#1524).
	fsyncOnFlush bool

	file      *mailindex.File // the wire-format snapshot
	keywords  keywordsHdr     // parsed keyword name registry
	filenames map[uint32]string
	sizes     map[uint32]uint32 // UID → message size in bytes, stored in .names sidecar

	// lineage pairs this base with its log; folded* say which log it absorbed
	// and how far. Zero predates the extension and proves nothing.
	lineage lineageHdr

	logSize int64     // byte count of .index.log after last write/reload
	baseMod time.Time // mtime of base .index at last full reload
	// baseIdent is compared by inode+device: coarse mtime hides a same-tick
	// replace, and reload()'s fast path would trust a stale snapshot.
	baseIdent os.FileInfo

	// logFD and namesFD stay open so an append costs one write(2). Callers must
	// closeFDs() before anything that replaces these files on disk.
	logFD   *os.File
	namesFD *os.File

	// dboxHdr is the folder GUID + flags from the dbox-hdr ext.
	hdr dboxHdr

	// vsize caches the hdr-vsize aggregate, kept in step with appends and
	// expunges and self-healing when {HighestUID, MessageCount} drift.
	vsize hdrVsize

	// traceID is the calling session's correlation ID. The index is shared, so
	// it names whichever session touched the folder last.
	traceID string

	// owner is the lock owner of the session that touched this folder last,
	// stamped beside traceID, so a BUSY names the holder (#1664).
	owner string
}

// closeFDs closes logFD and namesFD. Must run before anything replaces those
// files on disk, and when the folderState is evicted.
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

// shouldRotate applies the triple shared with the mdbox map log: never below
// minBytes, past minAge between the two, always above maxBytes.
func shouldRotate(logSize, minBytes, maxBytes int64, sinceLastFold, minAge time.Duration) bool {
	if minBytes == 0 {
		return false // compaction disabled
	}
	if logSize > maxBytes {
		return true
	}
	if logSize < minBytes {
		return false
	}
	return sinceLastFold >= minAge
}

// sinceLastFold takes the base .index mtime as the time of the last fold --
// the base is rewritten exactly then, so every process reads it the same, where
// a per-descriptor stamp would be zero on each fresh open and fold a
// reconnect-per-cycle workload on its first write. Unstattable reads as
// infinitely old, so an unexaminable base folds on size alone rather than
// never.
func (fs *folderState) sinceLastFold() time.Duration {
	mod := fs.baseMod
	if mod.IsZero() {
		st, err := os.Stat(fs.indexPath)
		if err != nil {
			return time.Duration(math.MaxInt64)
		}
		mod = st.ModTime()
	}
	return time.Since(mod)
}

// compactLogIfNeeded flushes and resets the log once it crosses the thresholds.
// Errors are non-fatal; the log stays larger. Caller holds fs.mu.
func (u *userIndex) compactLogIfNeeded(fs *folderState) {
	min := u.b.logCompactMinBytes
	if min == 0 {
		return // compaction disabled
	}
	if !shouldRotate(fs.logSize, min, u.b.logCompactMaxBytes, fs.sinceLastFold(), u.b.logCompactMinAge) {
		return
	}
	// Never flush our header if a compaction replaced the log since our reload:
	// a stale NextUID would regress the counter. Impossible under the lock.
	if fs.logFileReplaced() {
		slog.Warn("fileindex: skipping compaction, .log replaced since reload", "folder", fs.folder)
		fs.closeFDs()
		return
	}
	// Before the flush, because the floor has to be in the base this flush
	// writes: the truncate below takes the expunge records with it.
	if err := fs.stampExpungeFloorLocked(); err != nil {
		slog.Warn("fileindex: could not stamp the expunge floor; not compacting", "folder", fs.folder, "err", err)
		return
	}
	if err := fs.flush(false); err != nil {
		// Non-fatal but not silent: the log grows until every open replays it
		// (#1258/#1270), and the counter is what an operator alerts on (#1285).
		metricCompactionRefused.Inc()
		slog.Warn("fileindex: log compaction flush failed; rotation is not happening for this folder",
			"folder", fs.folder, "log_size", fs.logSize, "err", err)
		return
	}
	fs.closeFDs()
	if err := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); err != nil {
		slog.Warn("fileindex: log compaction truncate failed", "folder", fs.folder, "err", err)
		return
	}
	fs.logSize = 0
}

// fdMatchesFile reports whether f still refers to fi's file, by device+inode.
// Unprovable identity is "not the same file".
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

// logFileReplaced reports whether the on-disk .log differs by inode+device from
// the one fs.logFD holds -- another process renamed over it. Caller holds fs.mu.
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

// indexRootDir resolves the index root: INDEX= when set, else the mail root,
// else Home.
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

// diskName is the folder as it appears on disk. The mailbox driver applies the
// same rule: two spellings is a folder neither tree finds (#1586).
func (u *userIndex) diskName(folder string) string {
	if u.listUTF8 {
		return folder
	}
	return mboxenc.ToModUTF7(folder)
}

// indexDir is the per-folder index directory: the index root joined with the
// driver's folder sub-layout, so the index mirrors the mailbox tree.
func (u *userIndex) indexDir(folder string) string {
	return filepath.Join(u.indexRootDir(), mailbox.FolderSubpathEscaped(u.driver, folder, u.diskName(folder), u.separator, u.escapeChar))
}

// folderVolatileDir returns the per-folder volatile directory when
// volatileDir is configured, or "" when disabled.
func (u *userIndex) folderVolatileDir(folder string) string {
	if u.volatileDir == "" {
		return ""
	}
	return filepath.Join(u.volatileDir, mailbox.FolderSubpathEscaped(u.driver, folder, u.diskName(folder), u.separator, u.escapeChar))
}

// withFolderROUnlocked serves a read without the cross-process lock, when the
// state on disk can prove its own consistency: the base names the log it
// absorbed and how far, and the log names the base it belongs to (see
// lineage.go). Where it cannot -- an index written before the extension -- this
// falls back to the locked path, so the weaker file format keeps the stronger
// guarantee.
//
// Only readers whose answer goes to a client may use it. A read that decides
// what to rewrite or delete stays locked: a stale answer there does not become
// visible a moment later, it decides wrongly and the write lands anyway (#1249).
func (u *userIndex) withFolderROUnlocked(folderID uint64, fn func(*folderState) error) error {
	whole := time.Now()
	defer func() { metricReadSeconds.Observe(time.Since(whole).Seconds()) }()

	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	if !fs.canReadUnlocked() {
		// Counted apart: this is the migration not having reached this folder,
		// not a read that wanted the lock.
		return u.withFolderROSite(folderID, lockSiteFallback, fn)
	}
	reloadStart := time.Now()
	fs.mu.Lock()
	err := fs.reload()
	fs.mu.Unlock()
	observeReadPart("reload", time.Since(reloadStart))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	buildStart := time.Now()
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	ferr := fn(fs)
	observeReadPart("build", time.Since(buildStart))
	return ferr
}

// canReadUnlocked reports whether this folder's files carry the pairing a
// lock-free read stands on. Per read: a folder gains it mid-session.
func (fs *folderState) canReadUnlocked() bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.lineage.Lineage != lineageUnknown
}

// withFolderRO reloads the folder state under a SHARED distributed lock, then
// runs read-only fn against the settled in-memory snapshot. The lock keeps it
// from interleaving with another process's lock-holding compaction and loading
// a torn view into the shared folderState, which every later locked write would
// then trust as a baseline, regressing NextUID. Shared holders run concurrently
// and block only against an in-flight exclusive writer.
func (u *userIndex) withFolderRO(folderID uint64, fn func(*folderState) error) error {
	return u.withFolderROSite(folderID, lockSiteRead, fn)
}

// withFolderROSite is withFolderRO with the reason recorded: an acquisition
// from an open and one from an unprovable read cost the same and mean opposites.
func (u *userIndex) withFolderROSite(folderID uint64, site string, fn func(*folderState) error) error {
	whole := time.Now()
	defer func() { metricReadSeconds.Observe(time.Since(whole).Seconds()) }()

	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	// The lock part covers every trip to the service, release included: timing
	// only the acquisition leaves that, about as costly, unnamed.
	var reloadDur time.Duration
	lockStart := time.Now()
	err := u.withDistLock(fs, true, site, func() error {
		reloadStart := time.Now()
		fs.mu.Lock()
		defer fs.mu.Unlock()
		rerr := fs.reload()
		reloadDur = time.Since(reloadStart)
		observeReadPart("reload", reloadDur)
		return rerr
	})
	observeReadPart("lock", time.Since(lockStart)-reloadDur)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// fn only reads the in-memory snapshot; shared lock allows
	// concurrent readers without blocking writers.
	buildStart := time.Now()
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	ferr := fn(fs)
	observeReadPart("build", time.Since(buildStart))
	return ferr
}

// withFolderLock runs fn under the cross-process index lock. The HoldsResource()
// shortcut keeps an outer holder's per-message calls from deadlocking on it.
func (u *userIndex) withFolderLock(fs *folderState, fn func() error) error {
	return u.withDistLock(fs, false, lockSiteWrite, func() error {
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
func (u *userIndex) withDistLock(fs *folderState, shared bool, site string, fn func() error) error {
	if u.b.locker != nil {
		mode := lockMode(shared)
		key := locks.MailboxKey(u.username, fs.folder)
		if !u.b.locker.HoldsResource(key) {
			ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), site), 35*time.Second)
			defer cancel()
			t0 := time.Now()
			var lk locks.Lock
			var err error
			owner := fs.lockOwner(u.owner)
			if shared {
				lk, err = locks.AcquireShared(ctx, u.b.locker, key, owner, 30*time.Second)
			} else {
				lk, err = locks.Acquire(ctx, u.b.locker, key, owner, 30*time.Second)
			}
			metricLockWait.WithLabelValues(mode, site).Observe(time.Since(t0).Seconds())
			metricLockAcquired.WithLabelValues(mode, site).Inc()
			if err != nil {
				return fmt.Errorf("fileindex/lock %s: %w", fs.folder, err)
			}
			slog.Debug("fileindex: lock wait",
				"user", u.username, "folder", fs.folder, "shared", shared,
				"lock_wait_ms", time.Since(t0).Milliseconds())
			defer func() {
				released := time.Now()
				_ = u.b.locker.Unlock(ctx, lk.ID)
				metricLockRelease.WithLabelValues(mode, site).Observe(time.Since(released).Seconds())
			}()
		} else {
			// Already ours -- inside another operation that took the lock, so no
			// round trip. Counted apart so leaving the process has an answer.
			metricLockReentrant.WithLabelValues(mode, site).Inc()
		}
	}
	return fn()
}

// withTwoFolderLocks acquires both X locks in lexicographic order, so a rename
// cannot deadlock against another rename or a multi-folder driver operation.
func (u *userIndex) withTwoFolderLocks(folderA, folderB string, fn func() error) error {
	if u.b.locker == nil {
		return fn()
	}
	a, b := folderA, folderB
	if a > b {
		a, b = b, a
	}
	keyA := locks.MailboxKey(u.username, a)
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), lockSiteWrite), 35*time.Second)
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

// RenameFolder moves the index directory, taking both per-folder locks in
// lexicographic order so concurrent renames and writes cannot deadlock.
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
		// The identity moves with the folder: a rename keeps the UIDVALIDITY,
		// which is what a rename means to a client.
		if u.folders != nil {
			if err := u.folders.Rename(oldName, newName); err != nil {
				slog.Warn("fileindex: folder identity not moved with the rename",
					"user", u.username, "from", oldName, "to", newName, "err", err)
			}
		}
		return nil
	})
}

// folderTreeDir is the subtree owned solely by folder: for dbox that is
// indexDir()'s parent. Delete/Rename use it to reclaim the whole subtree.
func (u *userIndex) folderTreeDir(dir string) string {
	switch u.driver {
	case "mdbox", "sdbox", "dbox":
		return filepath.Dir(dir)
	}
	return dir
}

// DeleteFolder removes the index directory and its volatile twin after
// UserMailbox.Delete, so the index does not outlive it. Idempotent.
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
		// The identity goes with the folder: RFC 3501 §6.3.4 requires one
		// recreated under this name to look new, not to get the old number.
		if u.folders != nil {
			if err := u.folders.Remove(folder); err != nil {
				slog.Warn("fileindex: folder identity not removed with the folder",
					"user", u.username, "folder", folder, "err", err)
			}
		}
		return nil
	})
}

// ---- internal helpers shared across folder.go + legacy.go ----

// Flag bits mirroring mailindex.MailFlag, kept as their own constants for
// grep-ability and against a future rename there.
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

// seqSetContains reports whether uid falls in the set; an empty one matches
// everything, the "give me all records" idiom.
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

// On-disk filenames: yarilo writes the native names, and legacy canonical ones
// are read once at OpenFolder and renamed in place.
const (
	IndexFileName            = "yarilo.index"
	IndexLogFileName         = "yarilo.index.log"
	IndexNamesFileName       = "yarilo.index.names"
	LegacyIndexFileName      = "dovecot.index"
	LegacyIndexLogFileName   = "dovecot.index.log"
	LegacyIndexNamesFileName = "dovecot.index.names"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func indexPathFor(indexDir string) string { return filepath.Join(indexDir, IndexFileName) }

// namesPath is the .names sidecar path for an index directory.
func namesPath(indexDir string) string { return filepath.Join(indexDir, IndexNamesFileName) }

// errForeignIndexPresent says the legacy-named index is another
// implementation's. Not a failure: the caller's driver decides what it means.
var errForeignIndexPresent = errors.New("the legacy-named index is another implementation's")

// removeForeignIndexFiles unlinks the derived index files another
// implementation left in a maildir folder -- a maildir is served from its own
// files, so their index says nothing we need, and leaving it lets their tools
// read it as current. Their keyword file stays: it is the only record of what
// the letters in a filename mean until we write our own (#1600, #1601).
// Failures are logged, not returned -- a leftover that cannot be unlinked must
// not turn tidying into an outage.
func removeForeignIndexFiles(indexDir string) {
	for _, name := range []string{
		LegacyIndexFileName,
		LegacyIndexLogFileName,
		"dovecot.index.log.2",
		"dovecot.index.cache",
	} {
		path := filepath.Join(indexDir, name)
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("fileindex: could not remove a foreign index file", "path", path, "err", err)
			}
			continue
		}
		slog.Info("fileindex: removed a foreign index file a maildir folder does not need", "path", path)
	}
}

// migrateLegacyFilenames renames legacy canonical filenames to native ones,
// atomically and idempotently. Errors only on a partial rename.
func migrateLegacyFilenames(indexDir string) error {
	// Only ours: those are another implementation's current names, and renaming
	// on the name alone moves their index somewhere they cannot find (#1574).
	if legacyIndex := filepath.Join(indexDir, LegacyIndexFileName); fileExists(legacyIndex) &&
		looksForeign(legacyIndex) {
		return errForeignIndexPresent
	}
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
			// A concurrent opener may have renamed this pair; the loser sees
			// ENOENT, and nativePath existing means it is already done.
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

// loadNames reads the .names sidecar (TSV, legacy rows taken as size 0). A scan
// that stops early is an error: the next flush would rewrite from a partial map.
func loadNames(indexDir string) (map[uint32]string, map[uint32]uint32, error) {
	names := map[uint32]string{}
	sizes := map[uint32]uint32{}
	f, err := os.Open(namesPath(indexDir))
	if err != nil {
		if os.IsNotExist(err) {
			return names, sizes, nil
		}
		return names, sizes, fmt.Errorf("fileindex/names: open: %w", err)
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
	if err := sc.Err(); err != nil {
		return names, sizes, fmt.Errorf("fileindex/names: read: %w", err)
	}
	return names, sizes, nil
}

// requireName reports whether the caller supplied one. On an update the answer
// is refused rather than obeyed: erasing a name is the loss itself (#1693).
func requireName(where, folder string, uid uint32, filename string) bool {
	if filename != "" {
		return true
	}
	report(where, folder, uid, "no filename", "the message would be unreadable")
	return false
}

// report panics under test with the caller's file and line, and logs in
// production. The stack carries the frames above it.
func report(where, folder string, uid uint32, what, why string) {
	_, file, line, _ := runtime.Caller(3)
	if underTest() {
		panic(fmt.Sprintf("fileindex: %s of uid %d in %q with %s at %s:%d -- %s (#1693)",
			where, uid, folder, what, file, line, why))
	}
	slog.Error("fileindex: record written without what makes it readable",
		"where", where, "folder", folder, "uid", uid, "missing", what,
		"caller", fmt.Sprintf("%s:%d", file, line))
}

// underTest reports whether this is a `go test` binary. Looked up on the call:
// testing registers its flags after package variables run.
func underTest() bool { return flag.Lookup("test.v") != nil }

// appendName appends one entry to the .names sidecar through a kept-open fd, so
// it costs one write(2). loadNames takes the last entry for a uid.
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

// saveNames rewrites the .names sidecar from the records the index holds, name
// from memory else from disk: a record returns through the log, a name has none
// (#1693). A uid on disk the records lack is kept at or above nextUID (an
// append not replayed here), dropped below it as expunged. Under the folder's
// exclusive lock, the one appendName writes under.
func saveNames(indexDir, volatileDir, folder string, names map[uint32]string, sizes map[uint32]uint32, live map[uint32]struct{}, nextUID uint32) error {
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return fmt.Errorf("fileindex/names: mkdir: %w", err)
	}
	if volatileDir != "" {
		if err := os.MkdirAll(volatileDir, 0o700); err != nil {
			return fmt.Errorf("fileindex/names: mkdir volatile: %w", err)
		}
	}
	onDisk, diskSizes, err := loadNames(indexDir)
	if err != nil {
		return err
	}
	write := make(map[uint32]struct{}, len(live))
	for uid := range live {
		write[uid] = struct{}{}
	}
	for uid := range onDisk {
		if _, held := live[uid]; !held && uid >= nextUID {
			write[uid] = struct{}{}
		}
	}
	dst := namesPath(indexDir)
	tmp := sidecarTmpPath(dst, volatileDir)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/names: open tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	for uid := range write {
		name, size := names[uid], sizes[uid]
		if name == "" {
			name = onDisk[uid]
			if size == 0 {
				size = diskSizes[uid]
			}
		}
		// Logged, not refused: a migrated index arrives before anything has
		// named its records.
		if name == "" {
			slog.Error("fileindex: a record the index holds is named nowhere",
				"folder", folder, "uid", uid, "index_dir", indexDir)
		}
		fmt.Fprintf(bw, "%d\t%s\t%d\n", uid, name, size) //nolint:errcheck
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

// ensureLogStub writes an empty .log if none exists: the canonical reader fails
// hard when .index is present without it, even though Recreate never appends.
func ensureLogStub(indexPath, volatileDir string, indexID, lineage uint32) error {
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
	// The stub announces the base it belongs to: carrying the old constant it
	// would pair with nothing, leaving the folder unprovable until an append.
	hdr := mailindex.NewLogHeader(indexID, lineage, uint32(time.Now().Unix()))
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

// debugLog wraps slog.Debug so tests can quiet the package by raising the
// default level. A free function, so no receiver allocation.
func debugLog(msg string, kv ...any) { slog.Debug("fileindex: "+msg, kv...) }

// Cache-pair surface (#1030), forwarded like every other folder verb so the
// interface assertion in the IMAP layer sees it on the handle sessions hold.
func (h *userHandle) CachePairIdentity(folderID uint64) (uint32, uint32, bool, error) {
	return h.stamped(folderID).CachePairIdentity(folderID)
}

func (h *userHandle) CachePath(folderID uint64) (string, error) {
	return h.stamped(folderID).CachePath(folderID)
}

func (h *userHandle) SetCacheOffsets(folderID uint64, offsets map[uint32]uint32) error {
	return h.stamped(folderID).SetCacheOffsets(folderID, offsets)
}

// PurgeCache forwards the cache purge (#1030) like every other folder verb.
func (h *userHandle) PurgeCache(folderID uint64) (int, int64, error) {
	return h.stamped(folderID).PurgeCache(folderID)
}

// EnsureCacheExtension forwards the lazy add (#1184).
func (h *userHandle) EnsureCacheExtension(folderID uint64) (uint32, uint32, error) {
	return h.stamped(folderID).EnsureCacheExtension(folderID)
}

// BumpCacheGeneration forwards the generation bump (#1184).
func (h *userHandle) BumpCacheGeneration(folderID uint64) (uint32, error) {
	return h.stamped(folderID).BumpCacheGeneration(folderID)
}

// AdoptForeignNames satisfies mailbox.ForeignNameAdopter. On the handle as well
// as on the index, because callers hold the handle.
func (h *userHandle) AdoptForeignNames() error { return h.ui.AdoptForeignNames() }
