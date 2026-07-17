// Package mdbox is the multi-message dbox (mdbox) storage driver.
// Phase 5 rewrite — built on:
//
//   - internal/storage/mailbox/mdbox/mdboxmap  (map_uid + refcount keystone)
//   - internal/storage/mailindex                (Phase-1 binary index format)
//   - internal/storage/mailbox/dboxv2/format    (per-message dbox v2 wire layout)
//
// On-disk layout (per user):
//
//	<home>/mdbox/storage/
//	  m.<N>                   multi-message body file
//	  yarilo.map.index        the mdboxmap (legacy: dovecot.map.index)
//	<home>/mdbox/mailboxes/
//	  <folder>/               folder marker dir (per-folder index is the
//	                          external fileindex — mdbox does not duplicate
//	                          per-folder state inside this driver)
//
// "Filename" tokens surfaced to callers are the stringified map_uid.
// The external fileindex stores this in MessageMeta.Filename; the
// mdbox driver parses it back on Fetch / Remove / Copy.
//
// O(1) COPY: the driver implements the Copyable interface — copy
// increments the map record's refcount and returns the source
// filename unchanged; zero body bytes are read or written.
package mdbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mboxenc"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Directory layout constants.
const (
	mdboxRoot    = "mdbox"
	storageDir   = "storage"
	mailboxesDir = "mailboxes"
	dboxMailsDir = "dbox-Mails"
)

// dbox single-message wire constants (re-stated locally so this
// driver doesn't import dboxv2's unexported helpers).
const (
	dboxVersion       = 2
	messageHeaderSize = 32
	magicPreByte0     = 0x01
	magicPreByte1     = 0x02
	magicPost         = "\n\x01\x03\n"
)

// errCorruptRecord marks a record whose on-disk bytes are structurally
// unreadable (bad magic, unparseable size). Fetch maps it — together with a
// truncated read (io.EOF/ErrUnexpectedEOF) — onto mailbox.ErrCorruptStorage so
// the reactive rebuild can distinguish real corruption from a transient
// EIO/EACCES that must NOT trigger a rebuild.
var errCorruptRecord = errors.New("mdbox: corrupt record")

// Backend is the mdbox MailboxBackend factory. Per-user state
// lives in UserMailbox; the only thing the Backend holds is the
// shared locks.Locker and the optional alt-storage path template.
type Backend struct {
	locker         locks.Locker
	altStorageTmpl string        // base path template for cold-storage tier; "" = disabled
	writeSem       chan struct{} // nil = unlimited
	listUTF8       bool
	normalizeNFC   bool
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend. Every
// mutation path (Save, Remove, Copy) takes the strict
// map-then-folder lock chain: MdboxMapKey(user) first, then
// MailboxKey(user, folder).
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// WithAltStorage sets the base path template for the cold-storage
// tier. Supports %u/%n/%d/%Lu/%Ln/%Ld — same expansion as
// mail_home_template. Empty string disables alt storage.
//
// Example: "/mnt/cold/%d/%n"
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

// WithListUTF8 sets the on-disk folder name encoding.
// true (default): UTF-8. false: modified-UTF-7 (RFC 3501 §5.1.3) for legacy installations.
func WithListUTF8(v bool) Option { return func(b *Backend) { b.listUTF8 = v } }

// WithNormalizeNFC enables Unicode NFC normalization of folder names. Default true.
func WithNormalizeNFC(v bool) Option { return func(b *Backend) { b.normalizeNFC = v } }

// New constructs a Backend.
func New(opts ...Option) *Backend {
	b := &Backend{listUTF8: true, normalizeNFC: true}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	// Per-driver default: when no mail_path arrives from userdb, default to
	// <home>/mdbox. The resolved mailPath is the
	// mdbox root as-is — mdboxRoot() never re-appends a subdir.
	mailPath := u.MailPath
	if mailPath == "" {
		mailPath = filepath.Join(u.Home, mdboxRoot)
	}
	return &userMailbox{
		b:            b,
		home:         mailPath,
		indexRoot:    u.IndexDir,
		separator:    mailbox.SepOrDefault(u.Separator),
		username:     u.Username,
		owner:        makeOwner(u),
		altBasePath:  resolveAltBase(u.AltDir, b.altStorageTmpl, u.Username),
		listUTF8:     b.listUTF8,
		normalizeNFC: b.normalizeNFC,
	}
}

// resolveAltBase returns the expanded alt storage root for a user.
// perUser (from UserInfo.AltDir, already fully expanded) takes priority
// over the backend-level template so per-user userdb overrides work.
func resolveAltBase(perUser, tmpl, username string) string {
	if perUser != "" {
		return perUser
	}
	return expandAltPath(tmpl, username)
}

type userMailbox struct {
	b            *Backend
	home         string
	indexRoot    string // INDEX= target; "" means co-locate map index with payload
	separator    string // IMAP hierarchy separator; converted to "/" on disk (fs nesting)
	username     string
	owner        string
	altBasePath  string // expanded alt root + "/mdbox"; "" = disabled
	listUTF8     bool
	normalizeNFC bool

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

// mapStoragePath is where the mdbox map index (yarilo.map.index) lives: it
// follows INDEX= (index_root/storage) while the m.* payload stays in
// mail_path/storage. When INDEX= is unset it collapses back onto storagePath().
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
	if u.normalizeNFC {
		folder = mboxenc.NFC(folder)
	}
	if !u.listUTF8 {
		folder = mboxenc.ToModUTF7(folder)
	}
	return folder
}

func (u *userMailbox) folderPath(folder string) string {
	return filepath.Join(u.mdboxRoot(), mailbox.FolderSubpath("mdbox", folder, u.folderDiskName(folder), u.separator))
}

// folderDir is the mailbox directory itself (mailboxes/<name>), i.e. folderPath
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

// altStoragePath returns the alt-storage directory for m.<N> files.
// Mirrors primary storagePath() but rooted at altBasePath.
func (u *userMailbox) altStoragePath() string {
	return filepath.Join(u.altBasePath, storageDir)
}

// mfileAltPath returns the alt-storage path for m.<fileID>.
func (u *userMailbox) mfileAltPath(fileID uint32) string {
	return filepath.Join(u.altStoragePath(), fmt.Sprintf("m.%d", fileID))
}

// expandAltPath expands a path template (%u, %n, %d, %Lu, %Ln, %Ld)
// against a username ("localpart@domain").
// Returns "" when tmpl is empty (alt storage disabled).
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

// openMap ensures the per-user mdboxmap is open. Cached on the
// userMailbox for the session lifetime.
func (u *userMailbox) openMap() (*mdboxmap.Map, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mapping != nil {
		return u.mapping, nil
	}
	if err := os.MkdirAll(u.mapStoragePath(), 0o700); err != nil {
		return nil, fmt.Errorf("mdbox/openmap: mkdir: %w", err)
	}
	m, err := mdboxmap.Open(u.mapStoragePath(), u.username, mdboxmap.WithLocker(u.b.locker), mdboxmap.WithOwner(u.owner))
	if err != nil {
		return nil, err
	}
	u.mapping = m
	return m, nil
}

// withMailboxLock — folder-level X lock for per-folder ops
// (Create / Delete / Rename). Save / Fetch / Remove go through
// the map lock (taken inside mdboxmap).
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

func (u *userMailbox) Delete(folder string) error {
	return u.withMailboxLock(folder, func() error {
		if err := os.RemoveAll(u.folderDir(folder)); err != nil {
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
			if err := os.MkdirAll(filepath.Dir(u.folderDir(newName)), 0o700); err != nil {
				return fmt.Errorf("mdbox/rename: mkdir: %w", err)
			}
			if err := os.Rename(u.folderDir(oldName), u.folderDir(newName)); err != nil {
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
		if u.normalizeNFC {
			logical = mboxenc.NFC(logical)
		}
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

// rotateThreshold is the per-m.<N> size cap before Save rolls
// to a fresh file_id. Matches mdbox_rotate_size default (2 MiB).
const rotateThreshold uint32 = 2 * 1024 * 1024

// Save writes the message body into the user-wide multi-message
// store and records the location in the mdboxmap. Returns the
// assigned map_uid as a decimal string — the caller stores this
// in MessageMeta.Filename.
//
// Flow:
//
//  1. Build the dbox v2 record bytes.
//  2. Pick a destination m.<file_id>: current highest_file_id
//     unless adding `len(record)` to its size would exceed
//     rotateThreshold; in that case AllocFileID under the map
//     X lock to claim a fresh id atomically.
//  3. Open the m.<file_id> with O_APPEND, write the record,
//     capture the offset (file size before write).
//  4. AppendRecord(file_id, offset, size) under the map X lock
//     to allocate a fresh map_uid and persist the pointer.
//
// The folder-level lock is NOT taken here — concurrency between
// Save peers is serialised by the map X lock alone.
//
// uid parameter is the per-folder UID assigned by the external
// fileindex; mdbox ignores it (filename is map_uid, not the
// per-folder UID).
func (u *userMailbox) Save(folder string, r io.Reader, _ uint32, _ int64, _ []string) (string, error) {
	if u.b.writeSem != nil {
		u.b.writeSem <- struct{}{}
		defer func() { <-u.b.writeSem }()
	}
	body, err := readBodyCRLF(r)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: read body: %w", err)
	}
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir folder: %w", err)
	}
	if err := os.MkdirAll(u.storagePath(), 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir storage: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return "", err
	}

	guid := randomGUID()
	record := buildDboxRecord(body, guid)
	recLen := uint32(len(record))

	fileID := m.HighestFileID()
	if fileID == 0 {
		fileID = 1
	}
	curSize, _ := u.fileSize(u.mfilePath(fileID))
	if uint32(curSize)+recLen > rotateThreshold {
		fileID, err = m.AllocFileID()
		if err != nil {
			return "", fmt.Errorf("mdbox/save: alloc file id: %w", err)
		}
		curSize = 0
	}

	f, err := os.OpenFile(u.mfilePath(fileID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: open m.%d: %w", fileID, err)
	}
	// Re-stat under the file handle to nail down the offset we
	// actually wrote at. O_APPEND guarantees the bytes land at
	// the post-stat size, even if another process appended
	// between our stat() and the OpenFile().
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return "", fmt.Errorf("mdbox/save: stat handle: %w", err)
	}
	offset := uint32(st.Size())
	if _, err := f.Write(record); err != nil {
		f.Close()
		return "", fmt.Errorf("mdbox/save: write record: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("mdbox/save: close m.%d: %w", fileID, err)
	}

	mapUID, err := m.AppendRecord(fileID, offset, recLen, guid)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: map append: %w", err)
	}
	_ = curSize
	return strconv.FormatUint(uint64(mapUID), 10), nil
}

// Fetch resolves the message identified by filename (decimal map_uid)
// and returns a reader positioned at the body. altTier is the hint
// from MessageMeta.AltTier (persisted as FlagBackend in the index):
// when true the driver opens the alt-storage path directly, avoiding
// a wasted primary open() syscall for cold-tier messages.
func (u *userMailbox) Fetch(_, filename string, altTier bool) (io.ReadCloser, error) {
	mapUID, err := parseFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return nil, err
	}
	entry, ok, err := m.Lookup(mapUID)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: lookup: %w", err)
	}
	if !ok {
		// The folder index references a map_uid the map no longer carries — the
		// map and the fileindex have diverged, which is corruption.
		return nil, fmt.Errorf("mdbox/fetch: map_uid %d not found: %w", mapUID, mailbox.ErrCorruptStorage)
	}

	primary := u.mfilePath(entry.FileID)
	alt := u.mfileAltPath(entry.FileID)

	// Fast path: index flag tells us the tier — open alt directly. The alt hint
	// is best-effort: a stale flag or a corrupt alt copy falls through to primary
	// rather than surfacing as corruption.
	if altTier && u.AltEnabled() {
		if f, ferr := os.Open(alt); ferr == nil {
			body, berr := readRecordBody(f, entry.Offset)
			_ = f.Close()
			if berr == nil {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	}

	f, ferr := os.Open(primary)
	if ferr != nil {
		// Safety fallback: flag may lag reality if altmove ran before the
		// index was updated. Try alt before giving up.
		if errors.Is(ferr, os.ErrNotExist) && u.AltEnabled() {
			f, ferr = os.Open(alt)
		}
		if ferr != nil {
			// A vanished m.<N> the map still points at is corruption; any other
			// open error (EIO, EACCES) is transient and must not rebuild.
			if errors.Is(ferr, os.ErrNotExist) {
				return nil, fmt.Errorf("mdbox/fetch: open m.%d: %w: %w", entry.FileID, ferr, mailbox.ErrCorruptStorage)
			}
			return nil, fmt.Errorf("mdbox/fetch: open m.%d: %w", entry.FileID, ferr)
		}
	}
	body, err := readRecordBody(f, entry.Offset)
	_ = f.Close()
	if err != nil {
		return nil, corruptFetchErr(entry.FileID, err)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// corruptFetchErr classifies a record-read failure: a truncated read
// (io.EOF/ErrUnexpectedEOF) or a structurally bad record (errCorruptRecord) is
// corruption; anything else (EIO, EACCES) is a transient error that must not
// trigger a reactive rebuild.
func corruptFetchErr(fileID uint32, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errCorruptRecord) {
		return fmt.Errorf("mdbox/fetch: read m.%d: %w: %w", fileID, err, mailbox.ErrCorruptStorage)
	}
	return fmt.Errorf("mdbox/fetch: read m.%d: %w", fileID, err)
}

// Remove decrements the map record's refcount. Bytes stay on
// disk; purge reclaims them in Phase 6. Idempotent: a Remove of
// an already-zero-ref record is a no-op (UpdateRefcounts clamps
// at zero).
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

// Copy implements the Copyable optional interface for O(1)
// IMAP COPY. Returns the source filename unchanged — the
// destination folder stores the SAME map_uid under a fresh
// per-folder UID; only the refcount changes physically.
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

// List is intentionally empty — mdbox does not iterate its own
// directory to enumerate messages. The external fileindex is the
// per-folder source of truth (UID → filename → map_uid). Drivers
// that need a List override should rebuild from the index.
func (u *userMailbox) List(_ string) ([]*mailbox.MessageMeta, error) { return nil, nil }

// Scan walks every m.<N> physical file under the user's mdbox
// storage and yields one ScanRecord per stored message. The
// folder argument is ignored — mdbox storage is folder-agnostic,
// the per-folder fileindex is the source of truth for which
// folder owns each map_uid. Caller (admin rebuild) pairs this
// output with per-folder records to rebuild state.
//
// See rebuild.go (scanStorage / scanMFile) for implementation.
func (u *userMailbox) Scan(_ string) ([]mailbox.ScanRecord, error) {
	return u.scanStorage()
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

// ---- single-message dbox record (re-implemented here so this
// driver doesn't reach into dboxv2's unexported helpers) ------

// buildDboxRecord packs body into the canonical dbox v2 wire format
// used inside an m.<N> file. guid is embedded in the metadata
// trailer (G field). Callers that are saving a NEW message should
// supply a freshly-generated random GUID; callers compacting an
// EXISTING record (purge, altmove) must supply the original GUID
// from the source trailer so message identity is preserved.
func buildDboxRecord(body []byte, guid [16]byte) []byte {
	size := uint64(len(body))
	now := uint32(time.Now().Unix())

	var buf bytes.Buffer
	// File header line — "2 M20 C<stamp>\n"
	fmt.Fprintf(&buf, "%d M%x C%x\n", dboxVersion, messageHeaderSize, now)
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
	buf.WriteByte('\n')
	return buf.Bytes()
}

// readRecordBody seeks to offset, parses the file-header line
// and 32-byte message header, then returns the message body
// bytes.
func readRecordBody(f *os.File, offset uint32) ([]byte, error) {
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	// Consume the variable-length file-header line ("2 M20 C…\n").
	headerLine := make([]byte, 64)
	n, err := f.Read(headerLine)
	if err != nil {
		return nil, fmt.Errorf("read header line: %w", err)
	}
	lfIdx := bytes.IndexByte(headerLine[:n], '\n')
	if lfIdx < 0 {
		return nil, fmt.Errorf("file header line missing LF")
	}
	// Rewind past the consumed bytes, then re-seek to the start
	// of the 32-byte message header.
	if _, err := f.Seek(int64(offset)+int64(lfIdx)+1, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to message header: %w", err)
	}
	mh := make([]byte, messageHeaderSize)
	if _, err := io.ReadFull(f, mh); err != nil {
		return nil, fmt.Errorf("read message header: %w", err)
	}
	if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
		return nil, fmt.Errorf("%w: bad message magic", errCorruptRecord)
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: parse size: %v", errCorruptRecord, err)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// readRecordBodyAndTrailer reads both the message body and the
// metadata trailer in a single sequential pass over the file. It
// returns the body bytes and the GUID parsed from the G trailer
// field. Use this in compaction paths so the original GUID is
// preserved in the destination record — minting a fresh GUID would
// break per-message identity across purge/altmove cycles.
func readRecordBodyAndTrailer(f *os.File, offset uint32) (body []byte, guid [16]byte, err error) {
	if _, err = f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, guid, fmt.Errorf("seek: %w", err)
	}
	// Consume variable-length file-header line.
	headerLine := make([]byte, 64)
	n, err := f.Read(headerLine)
	if err != nil {
		return nil, guid, fmt.Errorf("read header line: %w", err)
	}
	lfIdx := bytes.IndexByte(headerLine[:n], '\n')
	if lfIdx < 0 {
		return nil, guid, fmt.Errorf("file header line missing LF")
	}
	// Seek to 32-byte message header.
	if _, err = f.Seek(int64(offset)+int64(lfIdx)+1, io.SeekStart); err != nil {
		return nil, guid, fmt.Errorf("seek to message header: %w", err)
	}
	mh := make([]byte, messageHeaderSize)
	if _, err = io.ReadFull(f, mh); err != nil {
		return nil, guid, fmt.Errorf("read message header: %w", err)
	}
	if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
		return nil, guid, fmt.Errorf("bad message magic")
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
	if err != nil {
		return nil, guid, fmt.Errorf("parse size: %w", err)
	}
	body = make([]byte, size)
	if _, err = io.ReadFull(f, body); err != nil {
		return nil, guid, fmt.Errorf("read body: %w", err)
	}
	// File position is now at the start of the trailer — parse it.
	_, parsed, _ := scanTrailer(f, 4096)
	return body, parsed.guid, nil
}

// readBodyCRLF reads r fully and ensures every line ends with
// CRLF (matches dbox v2 stream-conversion behaviour).
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
