// Package maildir implements MailboxBackend for Maildir format.
// Filename: {secs}.M{usecs}P{pid}_{seq}.{hostname}:2,{flags}
// uidlist: yarilo-uidlist v3
package maildir

import (
	"bufio"
	"context"
	"crypto/rand"
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

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Backend is the Maildir MailboxBackend factory.
// It holds only process-wide state (hostname, pid, counter).
// Per-user state lives in userMailbox.
type Backend struct {
	hostname string
	pid      int
	counter  atomic.Uint64
	locker   locks.Locker
}

// Option configures a Backend at construction time.
type Option func(*Backend)

// WithLocker wires a yarilo-locks client into the backend. Every shared-file
// write then takes a cross-process X lock on `mbox:<user>:<folder>` before
// mutating the on-disk maildir. A nil Locker disables cross-process locking
// (single-process tests / dev), keeping the in-process sync.Mutex only.
func WithLocker(l locks.Locker) Option {
	return func(b *Backend) { b.locker = l }
}

// New creates a Maildir backend.
func New(opts ...Option) *Backend {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	b := &Backend{
		hostname: hostname,
		pid:      os.Getpid(),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// OpenUser returns a per-session handle bound to u. The handle's Home field
// is used for all path resolution; usernames are never converted to paths here.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	return &userMailbox{
		b:          b,
		home:       u.Home,
		controlDir: u.ControlDir,
		altDir:     u.AltDir,
		username:   u.Username,
		owner:      makeOwner(u),
	}
}

// userMailbox is a per-session, per-user Maildir storage handle.
type userMailbox struct {
	b          *Backend
	home       string
	controlDir string // CONTROL= override root (empty = co-located with home)
	altDir     string // ALT= cold-tier root (empty = single-tier)
	username   string
	owner      string     // <process>/<pid>/<user> — passed to yarilo-locks for BUSY diagnostics
	mu         sync.Mutex // in-process fast-path; cross-process barrier is b.locker
}

// makeOwner builds the owner string for yarilo-locks BUSY reports.
// Format: "<process>/<pid>/<user>[/<sid>]". The optional session ID
// disambiguates concurrent sessions for the same user.
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

// withMailboxLock runs fn under both tiers — first the in-process mutex
// (cheap, serialises goroutines in this process), then the cross-process
// yarilo-locks X lock (slow path; only engaged when b.locker is non-nil).
// The lock TTL is conservatively long enough for any single write op; the
// context guards against backend unreachability.
func (u *userMailbox) withMailboxLock(folder string, fn func() error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.b.locker == nil {
		return fn()
	}
	key := locks.MailboxKey(u.username, folder)
	// Re-entrancy: if our Locker already holds this resource (an outer
	// scope took it for a batch — e.g. POP3 QUIT / multi-message IMAP
	// EXPUNGE), skip the per-call Acquire to avoid the same-owner BUSY
	// loop that yarilo-locks would otherwise produce.
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
	for _, sub := range []string{"INBOX/cur", "INBOX/new", "INBOX/tmp"} {
		if err := os.MkdirAll(filepath.Join(u.home, sub), 0o700); err != nil {
			return fmt.Errorf("maildir/init: %w", err)
		}
	}
	return nil
}

// Create provisions the cur/new/tmp triplet for a folder. Under the
// cross-process X lock so a concurrent Delete cannot tear the half-built
// tree apart and no sibling Create races on the same folder.
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

// Delete removes the entire folder tree (cur/new/tmp + uidlist + maildirfolder
// markers). Under the cross-process X lock so no in-flight delivery / read
// observes a half-deleted tree.
func (u *userMailbox) Delete(folder string) error {
	return u.withMailboxLock(folder, func() error {
		return os.RemoveAll(u.folderPath(folder))
	})
}

// Rename renames a folder on disk. Holds the cross-process X lock on BOTH
// the old and the new name in lexicographic order so two processes calling
// Rename simultaneously cannot deadlock against each other.
func (u *userMailbox) Rename(oldName, newName string) error {
	return u.withTwoMailboxLocks(oldName, newName, func() error {
		return os.Rename(u.folderPath(oldName), u.folderPath(newName))
	})
}

// withTwoMailboxLocks is the maildir twin of file/file.userIndex.withTwoMailboxLocks.
// Same lock-ordering convention so the maildir and index sides never
// deadlock against each other when Rename ripples through both backends.
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

// Save streams r into tmp/ then atomically renames into cur/.
// uid is the value returned by UserIndex.AllocateUID on this
// folder — Maildir does not encode it in the filename, but the
// uid→filename mapping is appended to the dovecot-uidlist sidecar
// inline so subsequent List() / Fetch() can resolve UIDs without
// a separate AppendUIDEntry call.
func (u *userMailbox) Save(folder string, r io.Reader, uid uint32, _ int64, flags []string) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("maildir: UID 0 invalid (call UserIndex.AllocateUID first)")
	}
	folderPath := u.folderPath(folder)
	now := time.Now()
	seq := u.b.counter.Add(1)
	basename := fmt.Sprintf("%d.M%dP%d_%d.%s",
		now.Unix(), now.UnixMicro()%1_000_000, u.b.pid, seq, u.b.hostname)

	tmpPath := filepath.Join(folderPath, "tmp", basename)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("maildir: create tmp: %w", err)
	}
	sc := &sizeCounter{}
	if _, err := io.Copy(f, io.TeeReader(r, sc)); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("maildir: write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	flagStr := encodeFlags(flags)
	// Dovecot filename convention: append ,S=<phys>,W=<virt> before :2,<flags>
	// so List() can return both sizes without reading the file body.
	finalName := fmt.Sprintf("%s,S=%d,W=%d:2,%s", basename, sc.phys, sc.phys+sc.lfNoCR, flagStr)

	if err := u.withMailboxLock(folder, func() error {
		dstPath := filepath.Join(folderPath, "cur", finalName)
		if err := os.Rename(tmpPath, dstPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("maildir: rename to cur: %w", err)
		}
		if err := u.appendUIDListLocked(folder, uid, finalName); err != nil {
			_ = os.Remove(dstPath)
			return fmt.Errorf("maildir: uidlist: %w", err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return finalName, nil
}

// appendUIDListLocked appends one entry to the yarilo-uidlist v3
// sidecar. Caller MUST hold the mailbox X lock.
func (u *userMailbox) appendUIDListLocked(folder string, uid uint32, filename string) error {
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
	_, err = fmt.Fprintf(f, "%d :%s\n", uid, filename)
	return err
}

// sizeCounter is an io.Writer that records bytes written and the number of LF
// bytes not preceded by CR (lone LFs that would gain a CR under CRLF
// normalisation).
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

func (u *userMailbox) Fetch(folder, filename string) (io.ReadCloser, error) {
	p := filepath.Join(u.folderPath(folder), "cur", filename)
	f, err := os.Open(p)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) || u.altDir == "" {
		return nil, fmt.Errorf("maildir: fetch %s: %w", filename, err)
	}
	// primary miss — try alt tier
	p = filepath.Join(u.altFolderPath(folder), "cur", filename)
	f, err = os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("maildir: fetch %s: %w", filename, err)
	}
	return f, nil
}

func (u *userMailbox) Remove(folder, filename string) error {
	p := filepath.Join(u.folderPath(folder), "cur", filename)
	err := os.Remove(p)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if u.altDir == "" {
		return nil
	}
	// not in primary — try alt tier
	p = filepath.Join(u.altFolderPath(folder), "cur", filename)
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// listCurEntries reads the cur/ directory of a folder path and returns
// message metadata keyed by filename. Returns nil on ErrNotExist.
func listCurEntries(dir string) (map[string]*mailbox.MessageMeta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	m := make(map[string]*mailbox.MessageMeta, len(entries))
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
		m[name] = &mailbox.MessageMeta{
			Filename: name,
			Flags:    flags,
			Keywords: keywords,
			Size:     sz,
			VSize:    virt,
		}
	}
	return m, nil
}

func (u *userMailbox) List(folder string) ([]*mailbox.MessageMeta, error) {
	primary, err := listCurEntries(filepath.Join(u.folderPath(folder), "cur"))
	if err != nil {
		return nil, err
	}

	// Merge alt-tier entries; primary takes precedence on name collision.
	if u.altDir != "" {
		alt, err := listCurEntries(filepath.Join(u.altFolderPath(folder), "cur"))
		if err != nil {
			return nil, err
		}
		for name, meta := range alt {
			if _, exists := primary[name]; !exists {
				if primary == nil {
					primary = make(map[string]*mailbox.MessageMeta)
				}
				primary[name] = meta
			}
		}
	}

	uidMap, err := u.readUIDList(folder)
	if err != nil {
		return nil, err
	}

	msgs := make([]*mailbox.MessageMeta, 0, len(primary))
	for name, meta := range primary {
		meta.UID = uidMap[name]
		msgs = append(msgs, meta)
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].UID < msgs[j].UID
	})
	return msgs, nil
}

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	_, err := os.Stat(u.folderPath(folder))
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if u.altDir == "" {
		return false, nil
	}
	_, err = os.Stat(u.altFolderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// collectFolderNames scans a maildir root and returns the folder
// names it contains (INBOX included). Returns nil on ErrNotExist.
func collectFolderNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var folders []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "INBOX" {
			folders = append(folders, "INBOX")
			continue
		}
		if strings.HasPrefix(name, ".") {
			folders = append(folders, strings.TrimPrefix(name, "."))
		}
	}
	return folders, nil
}

func (u *userMailbox) ListFolders() ([]string, error) {
	primary, err := collectFolderNames(u.home)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(primary)+1)
	result := make([]string, 0, len(primary)+1)
	// INBOX always present first
	seen["INBOX"] = true
	result = append(result, "INBOX")
	for _, f := range primary {
		if f == "INBOX" {
			continue
		}
		if !seen[f] {
			seen[f] = true
			result = append(result, f)
		}
	}

	if u.altDir != "" {
		alt, err := collectFolderNames(u.altDir)
		if err != nil {
			return nil, err
		}
		for _, f := range alt {
			if !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}
	return result, nil
}

// Scan walks the on-disk maildir for folder and returns one
// ScanRecord per message (cur/ + new/). Filenames carry both the
// flags (parsed) and size (from the optional "S=" infix or, when
// absent, from os.Stat); InternalDate comes from the file mtime.
//
// GUID is left as the zero value — Maildir filenames do not carry
// a stable per-message GUID; the index keeps that out of band.
// Caller (rebuild flow) is responsible for preserving the index's
// GUID assignment for filenames it matches against the old index.
func (u *userMailbox) Scan(folder string) ([]mailbox.ScanRecord, error) {
	roots := []string{u.folderPath(folder)}
	if u.altDir != "" {
		roots = append(roots, u.altFolderPath(folder))
	}
	seen := make(map[string]bool)
	out := make([]mailbox.ScanRecord, 0, 128)
	for _, root := range roots {
		for _, sub := range []string{"cur", "new"} {
			dir := filepath.Join(root, sub)
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
				if seen[name] {
					continue // primary takes precedence
				}
				seen[name] = true
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
				}
				if len(keywords) > 0 {
					rec.Flags = append(rec.Flags, keywords...)
				}
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

func (u *userMailbox) Close() error { return nil }

// ---- uidlist ---------------------------------------------------------------

// On-disk filenames. UIDListFileName is what we write; the
// legacy canonical name is renamed in place on first access so
// subsequent runs see only the yarilo file.
const (
	UIDListFileName       = "yarilo-uidlist"
	LegacyUIDListFileName = "dovecot-uidlist"
)

func (u *userMailbox) uidListPath(folder string) string {
	return filepath.Join(u.controlFolderPath(folder), UIDListFileName)
}

// migrateLegacyUIDList renames dovecot-uidlist → yarilo-uidlist
// if the yarilo-named file is absent. Idempotent.
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
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]uint32), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]uint32)
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
	}
	return m, sc.Err()
}

// ---- path helpers ----------------------------------------------------------

func (u *userMailbox) folderPath(folder string) string {
	if folder == "INBOX" {
		return filepath.Join(u.home, "INBOX")
	}
	return filepath.Join(u.home, "."+folder)
}

// controlFolderPath returns the directory for per-folder control files
// (yarilo-uidlist). When CONTROL= is configured, it uses controlDir as
// the root; otherwise mirrors folderPath under home.
func (u *userMailbox) controlFolderPath(folder string) string {
	root := u.home
	if u.controlDir != "" {
		root = u.controlDir
	}
	if folder == "INBOX" {
		return filepath.Join(root, "INBOX")
	}
	return filepath.Join(root, "."+folder)
}

// altFolderPath returns the cold-tier directory for a folder under altDir.
// Mirrors the same naming convention as folderPath.
func (u *userMailbox) altFolderPath(folder string) string {
	if folder == "INBOX" {
		return filepath.Join(u.altDir, "INBOX")
	}
	return filepath.Join(u.altDir, "."+folder)
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
