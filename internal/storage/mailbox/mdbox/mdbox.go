// Package mdbox is the multi-message dbox (mdbox) storage driver. Built on
// mdboxmap (map_uid + refcount), mailindex (binary index format), and the
// dbox v2 per-message wire layout.
//
// On-disk layout (per user):
//
//	<home>/mdbox/storage/
//	  m.<N>                   multi-message body file
//	  yarilo.map.index        the mdboxmap index
//	<home>/mdbox/mailboxes/
//	  <folder>/               folder marker dir (per-folder state lives in the
//	                          external fileindex, not duplicated here)
//
// Caller "filename" tokens are the stringified map_uid: the external fileindex
// stores it in MessageMeta.Filename, this driver parses it back on
// Fetch/Remove/Copy.
//
// COPY is O(1): it increments the map record's refcount and returns the source
// filename unchanged; no body bytes are read or written.
package mdbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Directory layout constants.
const (
	mdboxRoot    = "mdbox"
	storageDir   = "storage"
	mailboxesDir = "mailboxes"
	dboxMailsDir = "dbox-Mails"
)

// dbox single-message wire constants, re-stated locally to avoid importing
// dboxv2's unexported helpers.
const (
	dboxVersion       = 2
	messageHeaderSize = 32
	magicPreByte0     = 0x01
	magicPreByte1     = 0x02
	magicPost         = "\n\x01\x03\n"
)

// errCorruptRecord marks a record whose on-disk bytes are structurally
// unreadable (bad magic, unparseable size). Fetch maps it and truncated reads
// (io.EOF/ErrUnexpectedEOF) onto mailbox.ErrCorruptStorage, so real corruption
// is distinguished from a transient EIO/EACCES that must not trigger a rebuild.
var errCorruptRecord = errors.New("mdbox: corrupt record")

// Backend is the mdbox MailboxBackend factory. Per-user state lives in
// UserMailbox; the Backend holds only the shared locks.Locker and config.
type Backend struct {
	locker         locks.Locker
	altStorageTmpl string        // base path template for cold-storage tier; "" = disabled
	writeSem       chan struct{} // nil = unlimited
	listUTF8       bool

	// rotateSize is the per-m.<N> size cap before Save rolls to a fresh file_id
	// (mdbox_rotate_size). 0 selects defaultRotateSize (10 MiB default).
	rotateSize uint32

	// mapFormat is the on-disk map index format this deployment writes
	// (mdbox_map_format). Empty selects the package default.
	mapFormat mdboxmap.Format
	// rotateInterval rolls the append file once it is older than this, regardless
	// of size (mdbox_rotate_interval). 0 disables age-based rotation (the default).
	rotateInterval time.Duration
	// preallocate fallocate()s a new m.<N> to rotateSize up front (mdbox_preallocate_space).
	preallocate bool

	// logRotate* is the map log's rotation triple, forwarded to every map this
	// backend opens. logRotateSet distinguishes "not configured" from a
	// configured 0, which disables rotation.
	logRotateSet     bool
	logRotateMinSize int64
	logRotateMaxSize int64
	logRotateMinAge  time.Duration

	// now returns the current time; nil means time.Now. Injected by tests to
	// exercise age-based rotation deterministically without sleeping.
	now func() time.Time
}

// clock returns the backend's time source (time.Now unless a test injected one).
func (b *Backend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend. Lock order on every
// mutation path (Save, Remove, Copy): MdboxMapKey(user) then
// MailboxKey(user, folder).
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// WithAltStorage sets the base path template for the cold-storage tier.
// Supports %u/%n/%d/%Lu/%Ln/%Ld, same expansion as mail_home_template. Empty
// string disables alt storage. Example: "/mnt/cold/%d/%n".
func WithAltStorage(tmpl string) Option {
	return func(b *Backend) { b.altStorageTmpl = tmpl }
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

// WithListUTF8 sets the on-disk folder name encoding. true (default): UTF-8;
// false: modified-UTF-7 (RFC 3501 §5.1.3) for legacy installations.
func WithListUTF8(v bool) Option { return func(b *Backend) { b.listUTF8 = v } }

// WithRotateSize sets the per-m.<N> size cap (mdbox_rotate_size) before Save rolls
// to a fresh file_id. 0 selects the default (10 MiB).
func WithRotateSize(n uint32) Option { return func(b *Backend) { b.rotateSize = n } }

// WithLogRotation sets the map log's rotation triple
// (storage.mail_index_log_rotate_*), the same policy and the same values the
// folder file index rotates by. Unset leaves the map package's defaults.
func WithLogRotation(minSize, maxSize int64, minAge time.Duration) Option {
	return func(b *Backend) {
		b.logRotateSet = true
		// Zero means "leave this arm alone", so one knob can be set without
		// the operator having to restate the other two (#1481).
		if minSize != 0 {
			b.logRotateMinSize = minSize
		}
		if maxSize != 0 {
			b.logRotateMaxSize = maxSize
		}
		if minAge != 0 {
			b.logRotateMinAge = minAge
		}
	}
}

// WithMapFormat selects the on-disk map index format (mdbox_map_format). An
// empty value keeps the default; an unknown one is reported when the map is
// opened rather than silently falling back, because the value names how the
// bytes that locate every message are written.
func WithMapFormat(s string) Option {
	return func(b *Backend) {
		if s != "" {
			b.mapFormat = mdboxmap.Format(s)
		}
	}
}

// WithRotateInterval rolls the append file once it is older than d
// (mdbox_rotate_interval), independent of size. 0 disables age-based rotation.
func WithRotateInterval(d time.Duration) Option {
	return func(b *Backend) { b.rotateInterval = d }
}

// WithPreallocate enables fallocate() of a new m.<N> to the rotate size up front
// (mdbox_preallocate_space). Default false. A no-op on non-Linux builds.
func WithPreallocate(v bool) Option { return func(b *Backend) { b.preallocate = v } }

// New constructs a Backend.
func New(opts ...Option) *Backend {
	b := &Backend{listUTF8: true}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// rotateSizeOrDefault returns the configured rotate size, or 10 MiB when unset.
func (b *Backend) rotateSizeOrDefault() uint32 {
	if b.rotateSize == 0 {
		return 10 * 1024 * 1024
	}
	return b.rotateSize
}

// OpenUser returns a per-session handle bound to u.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	// When no mail_path arrives from userdb, default to <home>/mdbox. The
	// resolved mailPath is the mdbox root as-is; mdboxRoot() never re-appends a
	// subdir.
	mailPath := u.MailPath
	if mailPath == "" {
		mailPath = filepath.Join(u.Home, mdboxRoot)
	}
	return &userMailbox{
		b:           b,
		home:        mailPath,
		indexRoot:   u.IndexDir,
		separator:   mailbox.SepOrDefault(u.Separator),
		escapeChar:  u.StorageEscapeChar,
		username:    u.Username,
		owner:       makeOwner(u),
		altBasePath: resolveAltBase(u.AltDir, b.altStorageTmpl, u.Username),
		listUTF8:    b.listUTF8,
	}
}

// resolveAltBase returns the expanded alt storage root for a user. perUser
// (UserInfo.AltDir, already expanded) takes priority over the backend template
// so per-user userdb overrides work.
func resolveAltBase(perUser, tmpl, username string) string {
	if perUser != "" {
		return perUser
	}
	return expandAltPath(tmpl, username)
}

type userMailbox struct {
	b           *Backend
	home        string
	indexRoot   string // INDEX= target; "" means co-locate map index with payload
	separator   string // IMAP hierarchy separator; converted to "/" on disk (fs nesting)
	escapeChar  string // storage-name escape char; "" disables escaping
	username    string
	owner       string
	altBasePath string // expanded alt root + "/mdbox"; "" = disabled
	listUTF8    bool

	mu      sync.Mutex
	mapping *mdboxmap.Map // lazily opened on first use
}

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

// ---- path helpers ------------------------------------------

func (u *userMailbox) mdboxRoot() string   { return u.home }
func (u *userMailbox) storagePath() string { return filepath.Join(u.mdboxRoot(), storageDir) }

// mapStoragePath is where the map index (yarilo.map.index) lives: it follows
// INDEX= (index_root/storage) while the m.* payload stays in mail_path/storage.
// When INDEX= is unset it collapses onto storagePath().
func (u *userMailbox) mapStoragePath() string {
	if u.indexRoot != "" {
		return filepath.Join(u.indexRoot, storageDir)
	}
	return u.storagePath()
}
func (u *userMailbox) folderRoot() string {
	return filepath.Join(u.mdboxRoot(), mailboxesDir)
}
func (u *userMailbox) folderDiskName(folder string) string {
	// Escape first, encode second; the reverse path mirrors it (#1078). NFC is
	// applied once at the name-entry boundary (mailbox.NormalizeName), not
	// here, so folder arrives in its final form and there is no ordering of NFC
	// against escaping left to get wrong (#1113).
	folder = mailbox.EscapeLogicalName(folder, u.separator, "/", u.escapeChar)
	if !u.listUTF8 {
		folder = mboxenc.ToModUTF7(folder)
	}
	return folder
}

func (u *userMailbox) folderPath(folder string) string {
	return filepath.Join(u.mdboxRoot(), mailbox.FolderSubpath("mdbox", folder, u.folderDiskName(folder), u.separator))
}

// folderDir is the mailbox directory itself (mailboxes/<name>), folderPath
// without the trailing dbox-Mails leaf. Delete/Rename operate on this so the
// whole folder tree moves, not just its dbox-Mails marker.
func (u *userMailbox) folderDir(folder string) string {
	return filepath.Dir(u.folderPath(folder))
}
func (u *userMailbox) mfilePath(fileID uint32) string {
	return filepath.Join(u.storagePath(), fmt.Sprintf("m.%d", fileID))
}

// AltEnabled reports whether alt storage is configured for this user.
func (u *userMailbox) AltEnabled() bool { return u.altBasePath != "" }

// altStoragePath returns the alt-storage directory for m.<N> files: mirrors
// storagePath() but rooted at altBasePath.
func (u *userMailbox) altStoragePath() string {
	return filepath.Join(u.altBasePath, storageDir)
}

// mfileAltPath returns the alt-storage path for m.<fileID>.
func (u *userMailbox) mfileAltPath(fileID uint32) string {
	return filepath.Join(u.altStoragePath(), fmt.Sprintf("m.%d", fileID))
}

// expandAltPath expands a path template (%u, %n, %d, %Lu, %Ln, %Ld) against a
// username ("localpart@domain"). Returns "" when tmpl is empty.
func expandAltPath(tmpl, username string) string {
	if tmpl == "" {
		return ""
	}
	local, domain, _ := strings.Cut(username, "@")
	r := strings.NewReplacer(
		"%u", username,
		"%Lu", strings.ToLower(username),
		"%n", local,
		"%Ln", strings.ToLower(local),
		"%d", domain,
		"%Ld", strings.ToLower(domain),
	)
	return r.Replace(tmpl)
}

// openMap ensures the per-user mdboxmap is open, cached on the userMailbox for
// the session lifetime.
func (u *userMailbox) openMap() (*mdboxmap.Map, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mapping != nil {
		return u.mapping, nil
	}
	if err := os.MkdirAll(u.mapStoragePath(), 0o700); err != nil {
		return nil, fmt.Errorf("mdbox/openmap: mkdir: %w", err)
	}
	mapOpts := []mdboxmap.Option{
		mdboxmap.WithLocker(u.b.locker), mdboxmap.WithOwner(u.owner),
		mdboxmap.WithRotateSize(u.b.rotateSize), mdboxmap.WithFormat(u.b.mapFormat),
	}
	if u.b.logRotateSet {
		mapOpts = append(mapOpts, mdboxmap.WithLogRotation(
			u.b.logRotateMinSize, u.b.logRotateMaxSize, u.b.logRotateMinAge))
	}
	m, err := mdboxmap.Open(u.mapStoragePath(), u.username, mapOpts...)
	if err != nil {
		return nil, err
	}
	u.mapping = m
	return m, nil
}

// withMailboxLock takes the folder-level X lock for per-folder ops
// (Create/Delete/Rename). Save/Fetch/Remove instead go through the map lock
// taken inside mdboxmap.
func (u *userMailbox) withMailboxLock(folder string, fn func() error) error {
	if u.b.locker == nil {
		return fn()
	}
	key := locks.MailboxKey(u.username, folder)
	if u.b.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("mdbox/lock %s: %w", folder, err)
	}
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// withTwoMailboxLocks takes both per-folder X locks in lexicographic order so
// a concurrent A→B / B→A pair cannot deadlock.
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
			return fmt.Errorf("mdbox/lock %s: %w", a, err)
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
			return fmt.Errorf("mdbox/lock %s: %w", b, err)
		}
		defer func() { _ = u.b.locker.Unlock(ctx, lkB.ID) }()
	}
	return fn()
}

// ---- UserMailbox interface ---------------------------------

func (u *userMailbox) Init() error {
	if err := os.MkdirAll(u.storagePath(), 0o700); err != nil {
		return fmt.Errorf("mdbox/init: storage: %w", err)
	}
	if err := os.MkdirAll(u.folderPath("INBOX"), 0o700); err != nil {
		return fmt.Errorf("mdbox/init: INBOX: %w", err)
	}
	if _, err := u.openMap(); err != nil {
		return err
	}
	return nil
}

func (u *userMailbox) Create(folder string) error {
	return u.withMailboxLock(folder, func() error {
		if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
			return fmt.Errorf("mdbox/create: %w", err)
		}
		return nil
	})
}

// foldersRoot is the directory every folder lives under. folderDir("") resolves
// to it, which is what made an empty name remove all of the user's folders
// before names were validated above the drivers (#1069).
func (u *userMailbox) foldersRoot() string { return u.folderDir("") }

func (u *userMailbox) Delete(folder string) error {
	return u.withMailboxLock(folder, func() error {
		dir := u.folderDir(folder)
		// Last check before the removal, on the resolved path rather than the
		// name. Whatever was validated above, this must land on a folder and
		// not on the directory that holds them all.
		if err := mailbox.GuardDestructivePath(u.foldersRoot(), dir); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("mdbox/delete: %w", err)
		}
		return nil
	})
}

func (u *userMailbox) Rename(oldName, newName string) error {
	a, b := oldName, newName
	if a > b {
		a, b = b, a
	}
	return u.withMailboxLock(a, func() error {
		return u.withMailboxLock(b, func() error {
			from, to := u.folderDir(oldName), u.folderDir(newName)
			if err := mailbox.GuardDestructivePaths(u.foldersRoot(), from, to); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				return fmt.Errorf("mdbox/rename: mkdir: %w", err)
			}
			if err := os.Rename(from, to); err != nil {
				return fmt.Errorf("mdbox/rename %s → %s: %w", oldName, newName, err)
			}
			return nil
		})
	})
}

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	_, err := os.Stat(u.folderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (u *userMailbox) ListFolders() ([]mailbox.FolderEntry, error) {
	decode := func(name string) (string, bool) {
		logical := name
		if !u.listUTF8 {
			decoded, err := mboxenc.FromModUTF7(name)
			if err != nil {
				return "", false
			}
			logical = decoded
		}
		// Outermost on the way back: the escape sits at the logical-name
		// boundary, above modUTF7, so it is applied last here and first on the
		// way in (#1078). No NFC here: the disk name is already in the form the
		// boundary chose, and re-normalising it is the second owner this change
		// removed (#1113).
		logical = mailbox.UnescapeStorageName(logical, u.escapeChar)
		return logical, true
	}
	// A folder is selectable when it owns a dbox-Mails marker dir; a dir that
	// only holds child folders (an auto-created parent) is a \NoSelect
	// container. Payloads live in the shared storage/, so the marker dir stays
	// empty — it exists purely to record that the mailbox is selectable.
	root := u.folderRoot()
	isMarker := func(name string) bool { return name == dboxMailsDir }
	selectable := func(diskRel string) bool {
		_, err := os.Stat(filepath.Join(root, diskRel, dboxMailsDir))
		return err == nil
	}
	entries, err := mailbox.WalkDboxTree(root, u.separator, decode, isMarker, selectable)
	if err != nil {
		return nil, fmt.Errorf("mdbox/listfolders: %w", err)
	}
	return entries, nil
}

// driverName labels this driver in the metrics shared with the others: the
// question a save timing answers is comparative.
const driverName = "mdbox"

// Save writes the message body into the user-wide multi-message store and
// records its location in the mdboxmap. Returns the assigned map_uid as a
// decimal string; the caller stores it in MessageMeta.Filename.
//
// Flow:
//
//  1. Build the dbox v2 record bytes.
//  2. Pick a destination m.<file_id>: the current highest_file_id, unless
//     adding len(record) would exceed the rotate threshold, in which case
//     AllocFileID claims a fresh id under the map X lock.
//  3. Open m.<file_id> O_APPEND, write the record, capture the pre-write offset.
//  4. AppendRecord(file_id, offset, size) under the map X lock to allocate a
//     fresh map_uid and persist the pointer.
//
// The folder-level lock is not taken here; concurrent Save peers are serialised
// by the map X lock alone. The uid parameter (per-folder UID from the external
// fileindex) is ignored: the filename is the map_uid.
func (u *userMailbox) Save(folder string, r io.Reader, _ uint32, _ int64, _ []string, guid [16]byte) (string, uint32, [16]byte, error) {
	var noGUID [16]byte
	whole := time.Now()
	defer func() { mailboxmetrics.ObserveSave(driverName, time.Since(whole)) }()

	if u.b.writeSem != nil {
		// Waiting for a slot is somebody else's write, not this one's work,
		// and left unmeasured it would look like storage being slow.
		sem := time.Now()
		u.b.writeSem <- struct{}{}
		mailboxmetrics.ObserveSavePart(driverName, "sem", time.Since(sem))
		defer func() { <-u.b.writeSem }()
	}
	tRead := time.Now()
	body, err := readBodyCRLF(r)
	mailboxmetrics.ObserveSavePart(driverName, "read", time.Since(tRead))
	if err != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: read body: %w", err)
	}
	tPrepare := time.Now()
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: mkdir folder: %w", err)
	}
	if err := os.MkdirAll(u.storagePath(), 0o700); err != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: mkdir storage: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return "", 0, noGUID, err
	}

	// Zero guid means generate; a supplied guid is stored verbatim (RFC 8474 EMAILID stability).
	if guid == noGUID {
		guid = randomGUID()
	}
	msgRecord := buildDboxMessageRecord(body, guid, folder)
	recLen := uint32(len(msgRecord))

	fileID := m.HighestFileID()
	if fileID == 0 {
		fileID = 1
	}
	curSize, _ := u.fileSize(u.mfilePath(fileID))
	mailboxmetrics.ObserveSavePart(driverName, "prepare", time.Since(tPrepare))
	// Roll to a fresh file_id when appending would exceed the size cap, or when
	// the current append file is older than the rotate interval. The age check
	// uses a persisted create-time (not a filesystem btime) and a rolling window
	// (now - createTime > interval), so the file actually lived at least that long.
	nowT := u.b.clock()
	rotate := uint32(curSize)+recLen > u.b.rotateSizeOrDefault()
	if !rotate && u.b.rotateInterval > 0 {
		if ct, ok := m.CreateTime(fileID); ok && nowT.Sub(time.Unix(ct, 0)) > u.b.rotateInterval {
			rotate = true
		}
	}
	if rotate {
		// Rotation is periodic, not per-message, so it is reported apart: an
		// average that hides it reads as every save being slower than it is.
		tRotate := time.Now()
		fileID, err = m.AllocFileID()
		mailboxmetrics.ObserveSavePart(driverName, "rotate", time.Since(tRotate))
		if err != nil {
			return "", 0, noGUID, fmt.Errorf("mdbox/save: alloc file id: %w", err)
		}
		curSize = 0
	}

	tOpen := time.Now()
	f, err := os.OpenFile(u.mfilePath(fileID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: open m.%d: %w", fileID, err)
	}
	// Re-stat under the file handle to fix the write offset. O_APPEND guarantees
	// the bytes land at the post-stat size even if another process appended
	// between our earlier stat() and this OpenFile().
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return "", 0, noGUID, fmt.Errorf("mdbox/save: stat handle: %w", err)
	}
	offset := uint32(st.Size())
	mailboxmetrics.ObserveSavePart(driverName, "open", time.Since(tOpen))
	// The dbox file-header line is a file-level header: emit it only for the first
	// record in a new physical file (offset 0). Appended records start directly at
	// their message header, matching the dbox v2 layout.
	record := msgRecord
	if offset == 0 {
		record = append(buildDboxFileHeader(), msgRecord...)
		// Anchor the age clock for this file and, if configured, reserve its space
		// up front. Both are best-effort; a failure must not fail the Save.
		if rerr := m.RecordFileCreated(fileID, nowT.Unix()); rerr != nil {
			slog.Warn("mdbox: record file create-time failed", "user", u.username, "file_id", fileID, "err", rerr)
		}
		if u.b.preallocate {
			if perr := preallocateFile(f, int64(u.b.rotateSizeOrDefault())); perr != nil {
				slog.Warn("mdbox: preallocate failed", "user", u.username, "file_id", fileID, "err", perr)
			}
		}
	}
	recLen = uint32(len(record))
	tWrite := time.Now()
	_, werr := f.Write(record)
	mailboxmetrics.ObserveSavePart(driverName, "write", time.Since(tWrite))
	if werr != nil {
		f.Close()
		return "", 0, noGUID, fmt.Errorf("mdbox/save: write record: %w", werr)
	}
	// Close is its own part: on a networked filesystem this is where the
	// write is actually paid for, and folding it into the write above would
	// name the wrong step.
	tClose := time.Now()
	cerr := f.Close()
	mailboxmetrics.ObserveSavePart(driverName, "close", time.Since(tClose))
	if cerr != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: close m.%d: %w", fileID, cerr)
	}

	tMap := time.Now()
	mapUID, err := m.AppendRecord(fileID, offset, recLen, guid)
	mailboxmetrics.ObserveSavePart(driverName, "map", time.Since(tMap))
	if err != nil {
		return "", 0, noGUID, fmt.Errorf("mdbox/save: map append: %w", err)
	}
	_ = curSize
	return strconv.FormatUint(uint64(mapUID), 10), uint32(len(body)), guid, nil
}

// Fetch resolves the message identified by filename (decimal map_uid) and
// returns a reader positioned at the body. altTier (from MessageMeta.AltTier,
// persisted as FlagBackend in the index) opens the alt-storage path directly
// when true, avoiding a wasted primary open() for cold-tier messages.
func (u *userMailbox) Fetch(_, filename string, altTier bool) (io.ReadCloser, error) {
	mapUID, err := parseFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return nil, err
	}
	// Three parts with fixed boundaries, so the totals reconcile and the
	// comparison with maildir says which step costs: lookup resolves the map
	// entry (a freshness check included, when the map misses), open opens the
	// packed file, body seeks to the record and reads it. In maildir the first
	// two are one open by name and the third does not exist (#1205).
	lookupStart := time.Now()
	entry, ok, err := m.Lookup(mapUID)
	mdboxmap.ObserveReadPart("lookup", time.Since(lookupStart))
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: lookup: %w", err)
	}
	if !ok {
		// The folder index references a map_uid the map no longer carries: the
		// map and fileindex have diverged, which is corruption.
		//
		// Benign race: a stale session may FETCH a UID that a concurrent
		// purge just dropped (Remove only decrements the refcount; purge later
		// physically removes the zero-ref map record), which also surfaces as a
		// map miss. Rare and self-limiting: the marker is only
		// persisted for drivers with a reactive healer (mailbox.CanReactiveHeal),
		// which mdbox does not yet have, so it cannot produce a false FSCKD on a
		// healthy folder. A future healer must reconcile against the purge log
		// before acting on this signal.
		return nil, fmt.Errorf("mdbox/fetch: map_uid %d not found: %w", mapUID, mailbox.ErrCorruptStorage)
	}

	primary := u.mfilePath(entry.FileID)
	alt := u.mfileAltPath(entry.FileID)

	// Fast path: the index flag names the tier, so open alt directly. The hint is
	// best-effort: a stale flag or a corrupt alt copy falls through to primary
	// rather than surfacing as corruption.
	if altTier && u.AltEnabled() {
		openStart := time.Now()
		f, ferr := os.Open(alt)
		mdboxmap.ObserveReadPart("open", time.Since(openStart))
		if ferr == nil {
			bodyStart := time.Now()
			rc, berr := openRecordBody(f, entry.Offset)
			mdboxmap.ObserveReadPart("body", time.Since(bodyStart))
			if berr == nil {
				return rc, nil
			}
			_ = f.Close()
		}
	}

	openStart := time.Now()
	f, ferr := os.Open(primary)
	if ferr != nil {
		// Safety fallback: flag may lag reality if altmove ran before the
		// index was updated. Try alt before giving up.
		if errors.Is(ferr, os.ErrNotExist) && u.AltEnabled() {
			f, ferr = os.Open(alt)
		}
		if ferr != nil {
			mdboxmap.ObserveReadPart("open", time.Since(openStart))
			// A vanished m.<N> the map still points at is corruption; any other
			// open error (EIO, EACCES) is transient and must not trigger a rebuild.
			if errors.Is(ferr, os.ErrNotExist) {
				return nil, fmt.Errorf("mdbox/fetch: open m.%d: %w: %w", entry.FileID, ferr, mailbox.ErrCorruptStorage)
			}
			return nil, fmt.Errorf("mdbox/fetch: open m.%d: %w", entry.FileID, ferr)
		}
	}
	mdboxmap.ObserveReadPart("open", time.Since(openStart))
	bodyStart := time.Now()
	rc, err := openRecordBody(f, entry.Offset)
	mdboxmap.ObserveReadPart("body", time.Since(bodyStart))
	if err != nil {
		_ = f.Close()
		return nil, corruptFetchErr(entry.FileID, err)
	}
	return rc, nil
}

// openRecordBody positions f on the message body of the record at offset and
// returns a reader over exactly that body. It reads the record header only;
// the body is read by whoever consumes the reader, and only as far as they go.
//
// It used to read the whole body into memory first. A FETCH of
// HEADER.FIELDS on a 500 KB message then allocated and read 500 KB to hand
// back 2 KB, and under a one-CPU quota that garbage is what parked FETCH
// commands for whole seconds: the goroutine captured at a stall stood on the
// make([]byte, size) with GC at ~28% of the profile (#1517). maildir never
// had the problem because it returns the file itself.
//
// Truncation is still reported here, from Fetch, rather than surfacing later
// as an EOF on a reader nobody classifies: the body's end is checked against
// the file's size before the reader is returned.
func openRecordBody(f *os.File, offset uint32) (io.ReadCloser, error) {
	bodyOff, size, err := readRecordHeader(f, offset)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if bodyOff+int64(size) > st.Size() {
		return nil, fmt.Errorf("read body: %w", io.ErrUnexpectedEOF)
	}
	return &sectionCloser{
		SectionReader: io.NewSectionReader(f, bodyOff, int64(size)),
		f:             f,
	}, nil
}

// sectionCloser closes the file the section was cut from.
type sectionCloser struct {
	*io.SectionReader
	f *os.File
}

func (s *sectionCloser) Close() error { return s.f.Close() }

// readRecordHeader seeks to offset, skips the file-header line if present (the
// first record in a physical file carries it; legacy files carry it per
// record), parses the 32-byte message header, and returns where the body
// starts and how long it is.
func readRecordHeader(f *os.File, offset uint32) (bodyOff int64, size uint64, err error) {
	if _, err = f.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("seek: %w", err)
	}
	window := make([]byte, 64)
	n, err := f.Read(window)
	if err != nil {
		return 0, 0, fmt.Errorf("read record start: %w", err)
	}
	skip, ok := peekFileHeaderLen(window[:n])
	if !ok {
		return 0, 0, fmt.Errorf("%w: malformed record @%d", errCorruptRecord, offset)
	}
	hdrOff := int64(offset) + int64(skip)
	if _, err = f.Seek(hdrOff, io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("seek to message header: %w", err)
	}
	mh := make([]byte, messageHeaderSize)
	if _, err = io.ReadFull(f, mh); err != nil {
		return 0, 0, fmt.Errorf("read message header: %w", err)
	}
	if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
		return 0, 0, fmt.Errorf("%w: bad message magic", errCorruptRecord)
	}
	size, err = strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: parse size: %v", errCorruptRecord, err)
	}
	return hdrOff + int64(messageHeaderSize), size, nil
}

// corruptFetchErr classifies a record-read failure: a truncated read
// (io.EOF/ErrUnexpectedEOF) or a structurally bad record (errCorruptRecord) is
// corruption; anything else (EIO, EACCES) is transient and must not trigger a
// rebuild.
func corruptFetchErr(fileID uint32, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errCorruptRecord) {
		return fmt.Errorf("mdbox/fetch: read m.%d: %w: %w", fileID, err, mailbox.ErrCorruptStorage)
	}
	return fmt.Errorf("mdbox/fetch: read m.%d: %w", fileID, err)
}

// Remove decrements the map record's refcount. Bytes stay on disk; purge
// reclaims them later. Idempotent: a Remove of an already-zero-ref record is a
// no-op (UpdateRefcounts clamps at zero).
func (u *userMailbox) Remove(_, filename string) error {
	mapUID, err := parseFilename(filename)
	if err != nil {
		return fmt.Errorf("mdbox/remove: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return err
	}
	return m.UpdateRefcounts([]uint32{mapUID}, -1)
}

// Copy implements the optional Copyable interface for O(1) IMAP COPY. Returns
// the source filename unchanged: the destination folder stores the same map_uid
// under a fresh per-folder UID, and only the refcount changes on disk.
func (u *userMailbox) Copy(_, srcFilename, _ string, _ uint32) (string, error) {
	mapUID, err := parseFilename(srcFilename)
	if err != nil {
		return "", fmt.Errorf("mdbox/copy: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return "", err
	}
	if err := m.UpdateRefcounts([]uint32{mapUID}, +1); err != nil {
		return "", fmt.Errorf("mdbox/copy: refcount inc: %w", err)
	}
	return srcFilename, nil
}

// Move relocates a message between folders keeping its GUID (RFC 8474: MOVE
// must not change EMAILID). The record trailer names the owning folder, so this
// re-saves with the same GUID and unreferences the source. Both folder locks
// are taken in sorted order.
func (u *userMailbox) Move(srcFolder, dstFolder, filename string, guid [16]byte) (string, [16]byte, error) {
	var noGUID [16]byte
	mapUID, err := parseFilename(filename)
	if err != nil {
		return "", noGUID, fmt.Errorf("mdbox/move: %w", err)
	}
	var newName string
	outGUID := guid
	err = u.withTwoMailboxLocks(srcFolder, dstFolder, func() error {
		if outGUID == noGUID {
			// No caller-supplied id: the map record holds the effective GUID.
			m, merr := u.openMap()
			if merr != nil {
				return merr
			}
			entry, ok, lerr := m.Lookup(mapUID)
			if lerr != nil {
				return fmt.Errorf("mdbox/move: lookup: %w", lerr)
			}
			if !ok {
				return fmt.Errorf("mdbox/move: map_uid %d not found: %w", mapUID, mailbox.ErrCorruptStorage)
			}
			outGUID = entry.GUID
		}
		rc, ferr := u.Fetch(srcFolder, filename, false)
		if ferr != nil {
			return fmt.Errorf("mdbox/move: fetch: %w", ferr)
		}
		body, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			return fmt.Errorf("mdbox/move: read: %w", rerr)
		}
		name, _, saved, serr := u.Save(dstFolder, bytes.NewReader(body), 0, int64(len(body)), nil, outGUID)
		if serr != nil {
			return fmt.Errorf("mdbox/move: save: %w", serr)
		}
		if derr := u.Remove(srcFolder, filename); derr != nil {
			return fmt.Errorf("mdbox/move: remove source: %w", derr)
		}
		newName, outGUID = name, saved
		return nil
	})
	if err != nil {
		return "", noGUID, err
	}
	return newName, outGUID, nil
}

// List is intentionally empty: mdbox does not iterate its own directory to
// enumerate messages. The external fileindex is the per-folder source of truth
// (UID -> filename -> map_uid).
func (u *userMailbox) List(_ string) ([]*mailbox.MessageMeta, error) { return nil, nil }

// Scan walks every m.<N> physical file under the user's mdbox storage and
// yields one ScanRecord per stored message. The folder argument is ignored:
// mdbox storage is folder-agnostic, and the per-folder fileindex is the source
// of truth for which folder owns each map_uid. The admin rebuild pairs this
// output with per-folder records to rebuild state. See scanStorage/scanMFileAt.
func (u *userMailbox) Scan(_ string) ([]mailbox.ScanRecord, error) {
	return u.scanStorage()
}

// FolderAgnosticScan reports that mdbox Scan is storage-wide, so the per-folder
// rebuild path must reject it (see mailbox.FolderAgnosticStorage); the
// storage-wide rebuild is RebuildStorage.
func (u *userMailbox) FolderAgnosticScan() bool { return true }

// CompactMap folds the user's map log into its base. Exposed so an operator
// asking to fold this account's indexes gets the map too: it is the other
// structure replayed when a session opens, and the folder indexes do not
// contain it.
func (u *userMailbox) CompactMap() error {
	m, err := u.openMap()
	if err != nil {
		return err
	}
	return m.Compact()
}

// MapJournalSizes reports the on-disk size of the map's base index and append
// log, so a caller that folds the map can show what its call moved.
func (u *userMailbox) MapJournalSizes() (int64, int64, error) {
	m, err := u.openMap()
	if err != nil {
		return 0, 0, err
	}
	base, log := m.JournalSizes()
	return base, log, nil
}

// Close releases the cached map handle.
func (u *userMailbox) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mapping != nil {
		_ = u.mapping.Close()
		u.mapping = nil
	}
	return nil
}

// ---- single-message dbox record (re-implemented here to avoid
// reaching into dboxv2's unexported helpers) ------

// metaOrigMailbox is the trailer key for the mailbox a message was originally
// saved into. A storage-wide rebuild uses it to restore an orphan (a message no
// folder index references) to its home folder instead of guessing. It is an
// append-only key in the line-framed key/value trailer, so a reader that does
// not know the key skips it; the record size and every prior key's offset are
// unchanged.
const metaOrigMailbox = 'B'

// buildDboxFileHeader returns the dbox v2 file-header line ("2 M20 C<stamp>\n").
// It is a file-level header, written once at the start of each physical m.<N>
// file before its first message, never per message. C is the file creation
// timestamp.
func buildDboxFileHeader() []byte {
	return []byte(fmt.Sprintf("%d M%x C%x\n", dboxVersion, messageHeaderSize, uint32(time.Now().Unix())))
}

// buildDboxMessageRecord packs body into one canonical dbox v2 message record
// (32-byte message header, body, metadata trailer) without the file-header line
// (that belongs to the file; see buildDboxFileHeader). guid goes in the trailer
// G field: a fresh Save mints a random GUID, while compaction (purge/altmove)
// must pass the original GUID from the source trailer so message identity
// survives.
//
// origMailbox, when non-empty, is written as the metaOrigMailbox trailer key so
// a rebuild can route an orphaned copy back to its home folder. Compaction
// passes the value recovered from the source trailer; a fresh Save passes the
// destination folder.
func buildDboxMessageRecord(body []byte, guid [16]byte, origMailbox string) []byte {
	size := uint64(len(body))
	now := uint32(time.Now().Unix())

	var buf bytes.Buffer
	// 32-byte message header: magic + 'N' + spaces + size hex + LF.
	hdr := make([]byte, messageHeaderSize)
	for i := range hdr {
		hdr[i] = ' '
	}
	hdr[0] = magicPreByte0
	hdr[1] = magicPreByte1
	hdr[2] = 'N'
	copy(hdr[13:29], fmt.Sprintf("%016x", size))
	hdr[31] = '\n'
	buf.Write(hdr)
	buf.Write(body)
	// Metadata trailer.
	buf.WriteString(magicPost)
	fmt.Fprintf(&buf, "G%s\n", hex.EncodeToString(guid[:]))
	fmt.Fprintf(&buf, "R%x\n", now)
	fmt.Fprintf(&buf, "V%x\n", uint32(size))
	// Original mailbox (append-only; skipped by readers that don't know the key).
	// A folder name never contains a newline, so line framing is safe. An empty
	// origMailbox omits the key, indistinguishable from a pre-key record;
	// acceptable because no Save path passes an empty folder name.
	if origMailbox != "" {
		fmt.Fprintf(&buf, "%c%s\n", metaOrigMailbox, origMailbox)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// buildDboxRecord builds a file-header line followed by a single message record:
// the layout of a physical file holding exactly one message. Multi-message
// files carry the file-header line only once, before the first message. Retained
// for the single-record case and for tests that construct the legacy
// per-record-header layout the reader still accepts.
func buildDboxRecord(body []byte, guid [16]byte, origMailbox string) []byte {
	return append(buildDboxFileHeader(), buildDboxMessageRecord(body, guid, origMailbox)...)
}

// peekFileHeaderLen reports how many leading bytes of the record window belong
// to a dbox file-header line, and whether the window is well-formed. A message
// header begins with magicPreByte0 (0x01, never an ASCII digit); a file-header
// line begins with the ASCII version digit, so the first byte tells them apart:
//
//   - skip == 0: the record starts directly at its 32-byte message header (an
//     appended record in a multi-message file).
//   - skip  > 0: a file-header line of that length precedes the message header
//     (the first record in a physical file, or every record in a legacy
//     per-message-header file; both parse identically).
//
// ok == false means neither (no leading magic and no LF), i.e. a corrupt record.
// window must hold at least the start of the record.
func peekFileHeaderLen(window []byte) (skip int, ok bool) {
	if len(window) == 0 {
		return 0, false
	}
	if window[0] == magicPreByte0 {
		return 0, true
	}
	if lf := bytes.IndexByte(window, '\n'); lf >= 0 {
		return lf + 1, true
	}
	return 0, false
}

// readRecordBody returns the whole body of the record at offset. Kept for
// callers that genuinely need every byte; Fetch does not, and streams instead.
func readRecordBody(f *os.File, offset uint32) ([]byte, error) {
	bodyOff, size, err := readRecordHeader(f, offset)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(bodyOff, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to body: %w", err)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// readRecordBodyAndTrailer reads the message body and the metadata trailer in a
// single sequential pass, returning the body bytes, GUID and original mailbox
// from the trailer. Use it in compaction paths so the original GUID and
// orig-mailbox survive into the destination record; minting a fresh GUID or
// dropping the orig-mailbox would break message identity and orphan routing
// across purge/altmove cycles.
func readRecordBodyAndTrailer(f *os.File, offset uint32) (body []byte, guid [16]byte, origMailbox string, err error) {
	if _, err = f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, guid, "", fmt.Errorf("seek: %w", err)
	}
	// Skip the file-header line only when present.
	window := make([]byte, 64)
	n, err := f.Read(window)
	if err != nil {
		return nil, guid, "", fmt.Errorf("read record start: %w", err)
	}
	skip, ok := peekFileHeaderLen(window[:n])
	if !ok {
		return nil, guid, "", fmt.Errorf("malformed record @%d", offset)
	}
	if _, err = f.Seek(int64(offset)+int64(skip), io.SeekStart); err != nil {
		return nil, guid, "", fmt.Errorf("seek to message header: %w", err)
	}
	mh := make([]byte, messageHeaderSize)
	if _, err = io.ReadFull(f, mh); err != nil {
		return nil, guid, "", fmt.Errorf("read message header: %w", err)
	}
	if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
		return nil, guid, "", fmt.Errorf("bad message magic")
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
	if err != nil {
		return nil, guid, "", fmt.Errorf("parse size: %w", err)
	}
	body = make([]byte, size)
	if _, err = io.ReadFull(f, body); err != nil {
		return nil, guid, "", fmt.Errorf("read body: %w", err)
	}
	// The file position is now at the trailer; parse it. A parse error on a
	// compaction read means the destination copy loses its GUID and orig-mailbox
	// (a future orphan could never be restored), so log it rather than swallow it.
	_, parsed, terr := scanTrailer(f, 4096)
	if terr != nil {
		slog.Warn("mdbox: trailer parse failed during compaction; GUID/orig-mailbox lost for this copy",
			"file", f.Name(), "offset", offset, "err", terr)
	}
	return body, parsed.guid, parsed.origMailbox, nil
}

// readBodyCRLF reads r fully and ensures every line ends with CRLF (dbox v2
// stream-conversion behaviour).
func readBodyCRLF(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !needsCRLF(raw) {
		return raw, nil
	}
	out := make([]byte, 0, len(raw)+len(raw)/16)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '\n' && (i == 0 || raw[i-1] != '\r') {
			out = append(out, '\r', '\n')
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func needsCRLF(b []byte) bool {
	for i, c := range b {
		if c == '\n' && (i == 0 || b[i-1] != '\r') {
			return true
		}
	}
	return false
}

func parseFilename(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mdbox filename %q: %w", s, err)
	}
	return uint32(v), nil
}

func randomGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}
