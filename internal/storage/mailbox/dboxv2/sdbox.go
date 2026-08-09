package dboxv2

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Directory layout constants pinned by the wire spec; changing them breaks the
// on-disk format the rest of the storage layer depends on.
const (
	sdboxRoot         = "sdbox"
	mailboxesDir      = "mailboxes"
	dboxMailsDir      = "dbox-Mails"
	controlDir        = "control"
	uidvalidityFile   = "yarilo-uidvalidity"
	legacyUIDValidity = "dovecot-uidvalidity"
	temporaryPrefix   = ".temp."
	sdboxMailPrefix   = "u."
	uidvalidityFormat = "%08x" // 8 hex digits, lowercase
)

// Backend is the sdbox MailboxBackend factory; per-user state lives in
// userMailbox.
type Backend struct {
	hostname string
	pid      int
	tmpSeq   atomic.Uint64 // per-process counter for unique .temp.* names
	locker   locks.Locker
	writeSem chan struct{} // nil = unlimited
	listUTF8 bool
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend: every
// folder-mutating call (Save, Rename, Delete, Remove, AssignUID, Copy) takes
// the cross-process X lock on locks.MailboxKey(user, folder). A nil Locker
// keeps only the in-process sync.Mutex — never safe in production.
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

// New constructs an sdbox Backend.
func New(opts ...Option) *Backend {
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	b := &Backend{hostname: host, pid: os.Getpid(), listUTF8: true}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	// No mail_path from userdb: default to <home>/sdbox. The resolved mailPath
	// is the sdbox root as-is; sdboxRoot() never re-appends a subdir.
	mailPath := u.MailPath
	if mailPath == "" {
		mailPath = filepath.Join(u.Home, sdboxRoot)
	}
	return &userMailbox{
		b:          b,
		home:       mailPath,
		separator:  mailbox.SepOrDefault(u.Separator),
		escapeChar: u.StorageEscapeChar,
		username:   u.Username,
		owner:      makeOwner(u),
		listUTF8:   b.listUTF8,
	}
}

// userMailbox is a per-session sdbox storage handle.
type userMailbox struct {
	b          *Backend
	home       string
	separator  string // IMAP hierarchy separator; converted to "/" on disk (fs nesting)
	escapeChar string // storage-name escape char; "" disables escaping
	username   string
	owner      string
	listUTF8   bool
	mu         sync.Mutex
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

// HealCorruptFolder is the reactive self-heal: under the mailbox lock it
// expunges index records whose u.* file has vanished (targeted ExpungeMessage —
// QRESYNC tombstone + quota decrement, no ResetFolder, no UID assignment, so it
// cannot race a delivery), then clears the FSCKD marker in the SAME lock scope
// so a marker set by another process between scan and clear is not lost.
// Returns the expunged UIDs (the heal count) so the caller can invalidate their
// FTS documents. Called by the IMAP session when a folder carries the persisted
// FSCKD marker.
func (u *userMailbox) HealCorruptFolder(idx mailbox.UserIndex, folder *mailbox.Folder) ([]uint32, error) {
	var expunged []uint32
	err := u.withMailboxLock(folder.Name, func() error {
		var e error
		expunged, e = idxrebuild.ExpungeMissing(u, idx, folder)
		if e != nil {
			return e
		}
		if cm, ok := idx.(mailbox.CorruptionMarker); ok {
			return cm.ClearFolderCorrupt(folder.ID)
		}
		return nil
	})
	return expunged, err
}

// withMailboxLock runs fn under the per-process Mutex, then the cross-process X
// lock on locks.MailboxKey(user, folder). The HoldsResource short-circuit
// handles the POP3 QUIT re-entrancy pattern.
func (u *userMailbox) withMailboxLock(folder string, fn func() error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
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
		return fmt.Errorf("sdbox/lock %s: %w", folder, err)
	}
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// ---- Init / Create / Delete / Rename --------------------------

// Init materialises the per-user tree (control/, mailboxes/INBOX/dbox-Mails/)
// and seeds yarilo-uidvalidity if absent.
func (u *userMailbox) Init() error {
	if err := os.MkdirAll(u.controlPath(), 0o700); err != nil {
		return fmt.Errorf("sdbox/init: control: %w", err)
	}
	if err := os.MkdirAll(u.folderPath("INBOX"), 0o700); err != nil {
		return fmt.Errorf("sdbox/init: INBOX: %w", err)
	}
	if err := u.ensureUIDValidity(); err != nil {
		return err
	}
	return nil
}

// ensureUIDValidity creates control/yarilo-uidvalidity with the current unix
// timestamp on first run. A legacy control/dovecot-uidvalidity is renamed in
// place rather than reseeded — the stamp must never decrease across migration.
func (u *userMailbox) ensureUIDValidity() error {
	path := u.uidValidityPath()
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sdbox/uidvalidity: stat: %w", err)
	}
	legacy := filepath.Join(u.controlPath(), legacyUIDValidity)
	if _, err := os.Stat(legacy); err == nil {
		if rerr := os.Rename(legacy, path); rerr != nil {
			return fmt.Errorf("sdbox/uidvalidity: legacy rename: %w", rerr)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sdbox/uidvalidity: legacy stat: %w", err)
	}
	tmp := path + ".tmp"
	body := fmt.Sprintf(uidvalidityFormat, uint32(time.Now().Unix()))
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("sdbox/uidvalidity: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sdbox/uidvalidity: rename: %w", err)
	}
	return nil
}

// UIDValidity returns the per-user uidvalidity stamp, lazy-initialising the
// control file as needed. Used to seed a folder's index at create time.
func (u *userMailbox) UIDValidity() (uint32, error) {
	if err := u.ensureUIDValidity(); err != nil {
		return 0, err
	}
	body, err := os.ReadFile(u.uidValidityPath())
	if err != nil {
		return 0, fmt.Errorf("sdbox/uidvalidity: read: %w", err)
	}
	var v uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(string(body)), "%x", &v); err != nil {
		return 0, fmt.Errorf("sdbox/uidvalidity: parse %q: %w", body, err)
	}
	return v, nil
}

func (u *userMailbox) Create(folder string) error {
	return u.withMailboxLock(folder, func() error {
		if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
			return fmt.Errorf("sdbox/create: %w", err)
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
			return fmt.Errorf("sdbox/delete: %w", err)
		}
		return nil
	})
}

func (u *userMailbox) Rename(oldName, newName string) error {
	return u.withTwoMailboxLocks(oldName, newName, func() error {
		from, to := u.folderDir(oldName), u.folderDir(newName)
		if err := mailbox.GuardDestructivePaths(u.foldersRoot(), from, to); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return fmt.Errorf("sdbox/rename: mkdir: %w", err)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("sdbox/rename %s → %s: %w", oldName, newName, err)
		}
		return nil
	})
}

// withTwoMailboxLocks takes both per-folder X locks in lexicographic order.
// Matches the maildir / mdbox convention so renames cannot deadlock.
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
			return fmt.Errorf("sdbox/lock %s: %w", a, err)
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
			return fmt.Errorf("sdbox/lock %s: %w", b, err)
		}
		defer func() { _ = u.b.locker.Unlock(ctx, lkB.ID) }()
	}
	return fn()
}

// ---- Save (atomic publish) ----------------------------------

// Save streams r into a fresh .temp.* file then atomically renames it under the
// mailbox lock. The two-phase write is crash-safe: a partial body never appears
// under its final name. Returns the final basename for the caller to record via
// UserIndex.AppendMessage. flags are ignored — sdbox delegates flag storage to
// the index. A zero guid is generated here; a non-zero one is stored verbatim so
// EMAILID survives COPY/MOVE. The effective GUID is returned.
// driverName labels this driver in the timings shared with the others: it
// sits between the two, one lock per write and one file per message, which is
// what makes it the useful middle point in the comparison.
const driverName = "sdbox"

func (u *userMailbox) Save(folder string, r io.Reader, _ uint32, _ int64, _ []string, guid [16]byte) (string, uint32, [16]byte, error) {
	whole := time.Now()
	defer func() { mailboxmetrics.ObserveSave(driverName, time.Since(whole)) }()

	var noGUID [16]byte
	if u.b.writeSem != nil {
		u.b.writeSem <- struct{}{}
		defer func() { <-u.b.writeSem }()
	}
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return "", 0, noGUID, fmt.Errorf("sdbox/save: mkdir: %w", err)
	}

	body, err := readBodyCRLF(r)
	if err != nil {
		return "", 0, noGUID, fmt.Errorf("sdbox/save: read body: %w", err)
	}
	physSize := uint32(len(body))
	virtSize := physSize

	if guid == noGUID {
		guid = randomGUID()
	}
	now := uint32(time.Now().Unix())
	finalName := fmt.Sprintf("%s%s", sdboxMailPrefix, guidHex(guid))

	var buf bytes.Buffer
	buf.Write(encodeFileHeaderLine(now))
	buf.Write(encodeMessageHeader(messageHeader{Size: uint64(physSize)}))
	buf.Write(body)
	buf.Write(encodeMetadataBlock([]metadataEntry{
		{Key: metaKeyGUID, Value: guidHex(guid)},
		{Key: metaKeyReceived, Value: fmt.Sprintf("%x", now)},
		{Key: metaKeyVirtualSize, Value: fmt.Sprintf("%x", virtSize)},
	}))

	tempName := u.makeTempName()
	err = u.withMailboxLock(folder, func() error {
		dir := u.folderPath(folder)
		tempPath := filepath.Join(dir, tempName)
		finalPath := filepath.Join(dir, finalName)

		f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("sdbox/save: create %s: %w", tempPath, err)
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			_ = f.Close()
			_ = os.Remove(tempPath)
			return fmt.Errorf("sdbox/save: write: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("sdbox/save: close: %w", err)
		}
		if err := os.Rename(tempPath, finalPath); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("sdbox/save: rename %s → %s: %w", tempPath, finalPath, err)
		}
		return nil
	})
	if err != nil {
		return "", 0, noGUID, err
	}
	return finalName, virtSize, guid, nil
}

// Move relocates a message between folders by renaming the file; the GUID lives
// in the metadata block so it survives untouched (RFC 8474: MOVE keeps EMAILID).
// A zero guid is resolved from the source file's metadata.
func (u *userMailbox) Move(srcFolder, dstFolder, filename string, guid [16]byte) (string, [16]byte, error) {
	var noGUID [16]byte
	if err := os.MkdirAll(u.folderPath(dstFolder), 0o700); err != nil {
		return "", noGUID, fmt.Errorf("sdbox/move: mkdir dst: %w", err)
	}
	newName := filename
	err := u.withTwoMailboxLocks(srcFolder, dstFolder, func() error {
		srcPath := filepath.Join(u.folderPath(srcFolder), filename)
		if guid == noGUID {
			if g, _, _, merr := readMetadata(srcPath); merr == nil {
				guid = g
			}
		}
		dstPath := filepath.Join(u.folderPath(dstFolder), newName)
		if _, err := os.Lstat(dstPath); err == nil {
			// Name taken in the destination: mint one from the GUID.
			if guid == noGUID {
				return fmt.Errorf("sdbox/move: %s exists in %s and no guid to rename by", newName, dstFolder)
			}
			newName = fmt.Sprintf("%s%s", sdboxMailPrefix, guidHex(guid))
			dstPath = filepath.Join(u.folderPath(dstFolder), newName)
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("sdbox/move: rename %s → %s: %w", srcPath, dstPath, err)
		}
		return nil
	})
	if err != nil {
		return "", noGUID, err
	}
	return newName, guid, nil
}

// ---- Fetch / Remove / Copy ----------------------------------

// Fetch returns the message body between message_header end and
// metadata_magic_post; the file header and metadata are skipped. Caller MUST
// Close.
func (u *userMailbox) Fetch(folder, filename string, _ bool) (io.ReadCloser, error) {
	path := filepath.Join(u.folderPath(folder), filename)
	f, err := os.Open(path)
	if err != nil {
		// A vanished file is corruption (the index still references it); any
		// other open error (EIO, EACCES) is transient and must not rebuild.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("sdbox/fetch: open %s: %w: %w", path, err, mailbox.ErrCorruptStorage)
		}
		return nil, fmt.Errorf("sdbox/fetch: open %s: %w", path, err)
	}
	br := bufio.NewReader(f)
	if _, err := br.ReadBytes('\n'); err != nil {
		_ = f.Close()
		return nil, corruptRead("read file header", path, err)
	}
	hdrBuf := make([]byte, messageHeaderSize)
	if _, err := io.ReadFull(br, hdrBuf); err != nil {
		_ = f.Close()
		return nil, corruptRead("read message header", path, err)
	}
	mh, err := decodeMessageHeader(hdrBuf)
	if err != nil {
		// Malformed header (bad magic/version) is structural corruption.
		_ = f.Close()
		return nil, fmt.Errorf("sdbox/fetch %s: %w: %w", path, err, mailbox.ErrCorruptStorage)
	}
	bodyBytes := make([]byte, mh.Size)
	if _, err := io.ReadFull(br, bodyBytes); err != nil {
		_ = f.Close()
		return nil, corruptRead("read body", path, err)
	}
	_ = f.Close()
	return io.NopCloser(bytes.NewReader(bodyBytes)), nil
}

// corruptRead classifies a read error: a truncated file (EOF / unexpected EOF)
// is corruption; anything else is a transient I/O error that must not trigger a
// rebuild.
func corruptRead(op, path string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("sdbox/fetch: %s %s: %w: %w", op, path, err, mailbox.ErrCorruptStorage)
	}
	return fmt.Errorf("sdbox/fetch: %s %s: %w", op, path, err)
}

// Remove unlinks the message file. Idempotent: a missing file is not an error.
func (u *userMailbox) Remove(folder, filename string) error {
	return u.withMailboxLock(folder, func() error {
		path := filepath.Join(u.folderPath(folder), filename)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sdbox/remove: %w", err)
		}
		return nil
	})
}

// Copy hardlinks srcFilename into dstFolder under the destination's u.<dstUID>
// name — O(1) IMAP COPY: the inode is shared, only the directory entry changes.
// dstUID must have been allocated via UserIndex.AllocateUID on dstFolder.
//
// Optional API used by IMAP COPY when the backend type-asserts for Copyable;
// otherwise COPY falls back to read+Save.
func (u *userMailbox) Copy(srcFolder, srcFilename, dstFolder string, dstUID uint32) (string, error) {
	if dstUID == 0 {
		return "", fmt.Errorf("sdbox/copy: dstUID 0 invalid")
	}
	if err := os.MkdirAll(u.folderPath(dstFolder), 0o700); err != nil {
		return "", fmt.Errorf("sdbox/copy: mkdir dst: %w", err)
	}
	finalName := fmt.Sprintf("%s%d", sdboxMailPrefix, dstUID)
	srcPath := filepath.Join(u.folderPath(srcFolder), srcFilename)
	dstPath := filepath.Join(u.folderPath(dstFolder), finalName)
	err := u.withTwoMailboxLocks(srcFolder, dstFolder, func() error {
		if err := os.Link(srcPath, dstPath); err != nil {
			return fmt.Errorf("sdbox/copy: link %s → %s: %w", srcPath, dstPath, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return finalName, nil
}

// ---- List / FolderExists / ListFolders ---------------------

func (u *userMailbox) List(folder string) ([]*mailbox.MessageMeta, error) {
	entries, err := os.ReadDir(u.folderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sdbox/list: %w", err)
	}
	out := make([]*mailbox.MessageMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), sdboxMailPrefix) {
			continue
		}
		// Decimal suffix → UID in the name (old scheme). Hex GUID suffix →
		// UID=0; the file index is authoritative.
		uid64, _ := strconv.ParseUint(strings.TrimPrefix(e.Name(), sdboxMailPrefix), 10, 32)
		info, err := e.Info()
		if err != nil {
			continue
		}
		meta := &mailbox.MessageMeta{
			UID:          uint32(uid64),
			Filename:     e.Name(),
			Size:         uint32(info.Size()),
			InternalDate: info.ModTime(),
		}
		// GUID comes from the metadata block; the index records what we report.
		if guid, _, _, err := readMetadata(filepath.Join(u.folderPath(folder), e.Name())); err == nil {
			meta.GUID = guid
		}
		out = append(out, meta)
	}
	return out, nil
}

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	_, err := os.Stat(u.folderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ListFolders recursively walks mailboxes/ and surfaces every folder, nested
// included. A directory is selectable when it owns a dbox-Mails subdirectory;
// one holding only child folders is a \NoSelect container. The dbox-Mails leaf
// is a message store, so the walk neither descends into nor emits it.
func (u *userMailbox) ListFolders() ([]mailbox.FolderEntry, error) {
	root := u.mailboxesRoot()
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
	isMarker := func(name string) bool { return name == dboxMailsDir }
	selectable := func(diskRel string) bool {
		_, err := os.Stat(filepath.Join(root, diskRel, dboxMailsDir))
		return err == nil
	}
	entries, err := mailbox.WalkDboxTree(root, u.separator, decode, isMarker, selectable)
	if err != nil {
		return nil, fmt.Errorf("sdbox/listfolders: %w", err)
	}
	return entries, nil
}

// ---- Scan (rebuild contract) --------------------------------

// Scan returns one ScanRecord per u.<UID> file. The UID is parsed from the
// filename; GUID and InternalDate come from the on-disk metadata block. Used by
// the admin rebuild flow when the fileindex was lost — state recovers from
// filenames alone because the UID is in the name.
func (u *userMailbox) Scan(folder string) ([]mailbox.ScanRecord, error) {
	dir := u.folderPath(folder)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sdbox/scan: %w", err)
	}
	out := make([]mailbox.ScanRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), sdboxMailPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rec := mailbox.ScanRecord{
			Filename:     e.Name(),
			Size:         uint32(info.Size()),
			InternalDate: info.ModTime(),
		}
		// Best-effort metadata pull. Errors are non-fatal: the filename is
		// still surfaced so the index preserves the UID even for a corrupt body.
		if guid, vsize, when, err := readMetadata(filepath.Join(dir, e.Name())); err == nil {
			rec.GUID = guid
			if vsize > 0 {
				rec.VSize = vsize
			}
			if !when.IsZero() {
				rec.InternalDate = when
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// Close is a no-op: sdbox holds no long-lived resources (Fetch's fds are closed
// by the caller).
func (u *userMailbox) Close() error { return nil }

// ---- helpers -------------------------------------------------

func (u *userMailbox) sdboxRoot() string   { return u.home }
func (u *userMailbox) controlPath() string { return filepath.Join(u.sdboxRoot(), controlDir) }
func (u *userMailbox) uidValidityPath() string {
	return filepath.Join(u.controlPath(), uidvalidityFile)
}
func (u *userMailbox) mailboxesRoot() string {
	return filepath.Join(u.sdboxRoot(), mailboxesDir)
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
	return filepath.Join(u.sdboxRoot(), mailbox.FolderSubpath("sdbox", folder, u.folderDiskName(folder), u.separator))
}

// folderDir is the mailbox directory (mailboxes/<name>) — folderPath without
// the trailing dbox-Mails leaf. Delete/Rename operate on this so the whole
// folder tree moves, not just its payload.
func (u *userMailbox) folderDir(folder string) string {
	return filepath.Dir(u.folderPath(folder))
}

// makeTempName returns the pre-publish name
// ".temp.<sec>.P<pid>Q<seq>M<usec>.<host>".
func (u *userMailbox) makeTempName() string {
	now := time.Now()
	return fmt.Sprintf("%s%d.P%dQ%dM%d.%s",
		temporaryPrefix, now.Unix(), u.b.pid, u.b.tmpSeq.Add(1), now.Nanosecond()/1000, u.b.hostname)
}

// readBodyCRLF reads r fully and CRLF-terminates every line. Input already in
// CRLF is preserved as-is.
func readBodyCRLF(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// Fast path: already CRLF.
	if !needsCRLFNormalisation(raw) {
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

func needsCRLFNormalisation(b []byte) bool {
	for i, c := range b {
		if c == '\n' && (i == 0 || b[i-1] != '\r') {
			return true
		}
	}
	return false
}

// readMetadata walks past the file-header line + message header + body and
// returns the GUID, virtual size and received timestamp from the trailer.
// Errors propagate verbatim; callers fall back to filesystem-derived state.
func readMetadata(path string) ([16]byte, uint32, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	if _, err := br.ReadBytes('\n'); err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	hdrBuf := make([]byte, messageHeaderSize)
	if _, err := io.ReadFull(br, hdrBuf); err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	mh, err := decodeMessageHeader(hdrBuf)
	if err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	if _, err := br.Discard(int(mh.Size)); err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	entries, err := decodeMetadataBlock(br)
	if err != nil {
		return [16]byte{}, 0, time.Time{}, err
	}
	var guid [16]byte
	var vsize uint32
	var when time.Time
	for _, e := range entries {
		switch e.Key {
		case metaKeyGUID:
			if raw, derr := hexDecode(e.Value); derr == nil && len(raw) == 16 {
				copy(guid[:], raw)
			}
		case metaKeyVirtualSize:
			if v, derr := strconv.ParseUint(strings.TrimSpace(e.Value), 16, 32); derr == nil {
				vsize = uint32(v)
			}
		case metaKeyReceived:
			if v, derr := strconv.ParseInt(strings.TrimSpace(e.Value), 16, 64); derr == nil {
				when = time.Unix(v, 0).UTC()
			}
		}
	}
	return guid, vsize, when, nil
}

// hexDecode returns ErrUnexpectedEOF for an empty string, so metadata parse
// paths can treat an empty value as "field absent".
func hexDecode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, io.ErrUnexpectedEOF
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s)/2; i++ {
		var v byte
		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &v)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func randomGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}
