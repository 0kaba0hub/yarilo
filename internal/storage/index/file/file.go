// Package file is the per-folder index implementation that
// underlies every yarilo storage driver (maildir, dbox, mdbox).
//
// As of Phase 2 of the DOVECOT-STORAGE-COMPLIANCE rollout it is
// a thin adapter on top of internal/storage/mailindex — the
// on-disk format is byte-for-byte the canonical mail-index v7.3.
// The yarilo-specific .names sidecar persists for now as a
// transitional mechanism (Phase 3 sdbox drops the need for it by
// encoding UID in the filename; Phase 5 mdbox drops it entirely
// in favour of map_uid).
//
// The package exposes the same Backend / OpenUser / UserIndex
// surface every caller has used since v1.0. No consumer needs to
// change.
package file

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Backend is the per-process factory for fileindex handles. It
// holds only process-wide state; per-user state lives in
// userIndex (created by OpenUser).
type Backend struct {
	locker locks.Locker
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

// New constructs a Backend.
func New(opts ...Option) *Backend {
	b := &Backend{}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u. Handle
// methods take no user/path parameter — UserInfo is captured at
// open time (Dovecot mail_storage pattern).
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserIndex {
	return &userIndex{
		b:        b,
		home:     u.Home,
		username: u.Username,
		owner:    makeOwner(u),
		open:     make(map[uint64]*folderState),
	}
}

// userIndex is the per-user, per-session UserIndex implementation.
// Each (user, folder) pair gets a folderState lazily on first
// OpenFolder call; subsequent OpenFolder reuses cached state.
type userIndex struct {
	b        *Backend
	home     string
	username string
	owner    string

	mu    sync.Mutex
	next  uint64                  // monotonic per-session folder ID counter
	open  map[uint64]*folderState // folderID → state
	byDir map[string]uint64       // index dir path → folderID (dedup OpenFolder)
}

// folderState is the live in-memory snapshot of one folder's
// fileindex. Mutations update this struct then call Recreate to
// flush; Phase 2 deliberately uses pure-Recreate semantics for
// simplicity, with the log file kept empty (just a header).
//
// Phase 2.5 will add log-append support so write-heavy workloads
// don't pay full-file rewrite cost per mutation.
type folderState struct {
	mu sync.Mutex

	folder    string // mailbox folder name (e.g. "INBOX", "Sent")
	indexDir  string // <home>/<folder-relative>/
	indexPath string // <indexDir>/dovecot.index

	file      *mailindex.File // the wire-format snapshot
	keywords  keywordsHdr     // parsed keyword name registry
	filenames map[uint32]string

	// dboxHdr is the folder GUID + flags from the dbox-hdr ext.
	hdr dboxHdr
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

// IndexDirFor returns the per-folder directory layout this backend
// uses given a user home root: <home>/INBOX/ for INBOX, <home>/.<folder>/
// for others (Maildir convention; dbox/mdbox drivers piggyback on the
// same layout — Phase 3 will move sdbox to dbox-Mails/ subdir).
//
// Exposed because callers outside this package (notably
// internal/userstate/acl) also place control-state sidecars in this
// directory and must agree on the path. Keep this function as the
// single source of truth.
func IndexDirFor(home, folder string) string {
	if folder == "INBOX" {
		return filepath.Join(home, "INBOX")
	}
	return filepath.Join(home, "."+folder)
}

func (u *userIndex) indexDir(folder string) string {
	return IndexDirFor(u.home, folder)
}

// withFolderLock runs fn under the cross-process index lock for
// the supplied folder state. When no locker is wired (tests),
// fn runs unguarded.
//
// The HoldsResource() shortcut is preserved here so the POP3 QUIT
// pattern (outer caller takes the lock then drives per-message
// storage calls that touch the same key) does not deadlock against
// itself. The cross-goroutine race that arises when two goroutines
// on the same locks client see each other's holds-map state is a
// known limitation tracked in TODO.md and will be fixed by
// goroutine-local re-entrancy in a pkg/locks follow-up.
func (u *userIndex) withFolderLock(fs *folderState, fn func() error) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if u.b.locker == nil {
		return fn()
	}
	key := locks.MailboxKey(u.username, fs.folder)
	if u.b.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("fileindex/lock %s: %w", fs.folder, err)
	}
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
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
// has no long-lived file descriptors (pure Recreate path), so
// Close is currently a no-op besides clearing the map.
func (u *userIndex) Close() error {
	u.mu.Lock()
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
		oldDir := u.indexDir(oldName)
		newDir := u.indexDir(newName)
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
				delete(u.open, id)
			}
		}
		if u.byDir != nil {
			delete(u.byDir, oldDir)
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
			return fmt.Errorf("fileindex/migrate: rename %s → %s: %w", legacyPath, nativePath, err)
		}
	}
	return nil
}

// loadNames reads the .names sidecar into a UID→filename map.
// Missing file = empty map (not an error). Format is TSV:
//
//	<uid>\t<filename>\n
//
// Phase 2 keeps this as a yarilo-only sidecar; Phase 3 drops it
// for sdbox (UID is the filename), Phase 5 drops it for mdbox
// (map_uid replaces filenames altogether).
func loadNames(indexDir string) map[uint32]string {
	out := map[uint32]string{}
	f, err := os.Open(namesPath(indexDir))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		uid64, err := strconv.ParseUint(line[:tab], 10, 32)
		if err != nil {
			continue
		}
		out[uint32(uid64)] = line[tab+1:]
	}
	return out
}

// saveNames rewrites the .names sidecar atomically (.tmp +
// rename). Called from inside Recreate flush so concurrent
// readers always see a consistent view paired with the .index
// they just wrote.
func saveNames(indexDir string, names map[uint32]string) error {
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return fmt.Errorf("fileindex/names: mkdir: %w", err)
	}
	dst := namesPath(indexDir)
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/names: open tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	for uid, name := range names {
		fmt.Fprintf(bw, "%d\t%s\n", uid, name) //nolint:errcheck
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
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/names: rename: %w", err)
	}
	return nil
}

// ensureLogStub writes an empty .log file (just LogHeader) if
// none exists. Required because the canonical reader fails
// hard when .index is present but its .log sibling is missing.
// Pure-Recreate mode never appends to the log, but the file
// must exist for compat.
func ensureLogStub(indexPath string, indexID uint32) error {
	logPath := indexPath + ".log"
	if _, err := os.Stat(logPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileindex/log stub: stat: %w", err)
	}
	tmp := logPath + ".tmp"
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
	if err := os.Rename(tmp, logPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log stub: rename: %w", err)
	}
	return nil
}

// debugLog wraps slog.Debug calls so the package can quiet down
// in tests by setting slog default's level to LevelInfo. Kept as
// a free function so it has no method-receiver allocation.
func debugLog(msg string, kv ...any) { slog.Debug("fileindex: "+msg, kv...) }
