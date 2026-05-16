// Package mdbox implements MailboxBackend for the mdbox (multi-message dbox) format.
// Messages are stored in shared m.<file_id> files under mdbox-storage/; each folder
// keeps a dbox.map text file that maps into those files by (file_id, offset).
package mdbox

import (
	"bufio"
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
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

const (
	magicPre  = "\x01\x02"
	magicPost = "\n\x01\x03\n"
	rotateAt  = int64(10 * 1024 * 1024)
)

// Backend is the mdbox MailboxBackend factory.
// It holds no per-user state; all per-user state (file rotation, locks) lives
// in userMailbox, which is created fresh by OpenUser for each session.
type Backend struct {
	// rotateThreshold overrides the 10 MB rotation limit in tests.
	rotateThreshold int64
}

// New creates an mdbox backend.
func New() *Backend {
	return &Backend{rotateThreshold: rotateAt}
}

// OpenUser returns a per-session handle bound to u.
// File rotation state (currentFileID, currentSize) is per-handle so that
// concurrent sessions for the same user each hold their own state.
// The underlying mdbox-storage files are shared on disk (append-only), so
// concurrent writes are safe at the OS level but the per-handle state may
// diverge after rotation — correct for the current single-backend-per-user
// deployment; revisit when multi-node delivery is introduced (Phase 5).
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	return &userMailbox{b: b, home: u.Home}
}

// userMailbox is a per-session, per-user mdbox storage handle.
type userMailbox struct {
	b    *Backend
	home string

	mu            sync.Mutex
	currentFileID uint32
	currentSize   int64
	initDone      bool
}

// Init creates the INBOX folder and scans the user's mdbox-storage to resume
// from the highest existing file_id. Must be called before Save.
func (u *userMailbox) Init() error {
	if err := os.MkdirAll(u.folderPath("INBOX"), 0o700); err != nil {
		return fmt.Errorf("mdbox/init: %w", err)
	}

	storageDir := u.storageDir()
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return fmt.Errorf("mdbox/init: mkdir storage: %w", err)
	}

	// Scan for the highest existing m.<id> file to resume the sequence.
	entries, err := os.ReadDir(storageDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mdbox/init: scan storage: %w", err)
	}
	var maxID uint32
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "m.") {
			continue
		}
		id64, parseErr := strconv.ParseUint(e.Name()[2:], 10, 32)
		if parseErr != nil {
			continue
		}
		if uint32(id64) > maxID {
			maxID = uint32(id64)
		}
	}
	u.mu.Lock()
	if maxID > 0 {
		u.currentFileID = maxID
	} else {
		u.currentFileID = 1
	}
	u.currentSize = 0
	u.initDone = true
	u.mu.Unlock()
	return nil
}

func (u *userMailbox) Create(folder string) error {
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return fmt.Errorf("mdbox/create: %w", err)
	}
	return nil
}

func (u *userMailbox) Delete(folder string) error {
	if err := os.RemoveAll(u.folderPath(folder)); err != nil {
		return fmt.Errorf("mdbox/delete: %w", err)
	}
	return nil
}

func (u *userMailbox) Rename(oldName, newName string) error {
	if err := os.Rename(u.folderPath(oldName), u.folderPath(newName)); err != nil {
		return fmt.Errorf("mdbox/rename: %w", err)
	}
	return nil
}

// Save writes a message into the mdbox storage and appends a map entry.
// Returns "<file_id>:<offset>" as the filename token.
func (u *userMailbox) Save(folder string, r io.Reader, _ int64, _ []string) (string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: read body: %w", err)
	}

	guid := randomGUID()
	now := uint32(time.Now().Unix())
	physSize := uint32(len(body))
	virtSize := physSize

	record := buildRecord(body, guid, now, physSize, virtSize)
	recLen := int64(len(record))

	storageDir := u.storageDir()
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir storage: %w", err)
	}
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir folder: %w", err)
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	// Lazy-initialise currentSize from the actual file on first Save after Init.
	if u.currentSize == 0 && u.initDone {
		fi, statErr := os.Stat(u.mfilePath(storageDir, u.currentFileID))
		if statErr == nil {
			u.currentSize = fi.Size()
		}
	}

	if u.currentSize > u.b.rotateThreshold {
		u.currentFileID++
		u.currentSize = 0
	}

	fileID := u.currentFileID
	offset := u.currentSize

	mpath := u.mfilePath(storageDir, fileID)
	f, err := os.OpenFile(mpath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: open mfile: %w", err)
	}
	if _, err := f.Write(record); err != nil {
		f.Close()
		return "", fmt.Errorf("mdbox/save: write record: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("mdbox/save: close mfile: %w", err)
	}
	u.currentSize += recLen

	mapLine := fmt.Sprintf("%d %d %d 0\n", fileID, offset, physSize)
	mapPath := u.mapPath(folder)
	mf, err := os.OpenFile(mapPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("mdbox/save: open dbox.map: %w", err)
	}
	if _, err := fmt.Fprint(mf, mapLine); err != nil {
		mf.Close()
		return "", fmt.Errorf("mdbox/save: write dbox.map: %w", err)
	}
	if err := mf.Close(); err != nil {
		return "", fmt.Errorf("mdbox/save: close dbox.map: %w", err)
	}

	return fmt.Sprintf("%d:%d", fileID, offset), nil
}

// Fetch opens the message identified by "<file_id>:<offset>" and returns a
// reader positioned at the start of the raw RFC 5322 body.
func (u *userMailbox) Fetch(folder, filename string) (io.ReadCloser, error) {
	fileID, offset, err := parseFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: %w", err)
	}

	storageDir := u.storageDir()
	mpath := u.mfilePath(storageDir, fileID)
	f, err := os.Open(mpath)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: open mfile: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("mdbox/fetch: seek: %w", err)
	}

	size, err := parseHeader(f)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: parse header: %w", err)
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("mdbox/fetch: read body: %w", err)
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

// Remove marks the dbox.map entry for "<file_id>:<offset>" as expunged (lazy delete).
func (u *userMailbox) Remove(folder, filename string) error {
	fileID, offset, err := parseFilename(filename)
	if err != nil {
		return fmt.Errorf("mdbox/remove: %w", err)
	}

	mapPath := u.mapPath(folder)
	lines, err := readMapLines(mapPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mdbox/remove: read map: %w", err)
	}

	changed := false
	for i, rec := range lines {
		if rec.fileID == fileID && rec.offset == offset && rec.expunged == 0 {
			lines[i].expunged = 1
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeMapLines(mapPath, lines)
}

func (u *userMailbox) List(folder string) ([]*mailbox.MessageMeta, error) {
	mapPath := u.mapPath(folder)
	lines, err := readMapLines(mapPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mdbox/list: %w", err)
	}

	storageDir := u.storageDir()

	type entry struct {
		fileID uint32
		offset uint32
		meta   *mailbox.MessageMeta
	}
	var entries []entry

	for _, rec := range lines {
		if rec.expunged != 0 {
			continue
		}

		mpath := u.mfilePath(storageDir, rec.fileID)
		f, openErr := os.Open(mpath)
		if openErr != nil {
			return nil, fmt.Errorf("mdbox/list: open mfile: %w", openErr)
		}
		if _, seekErr := f.Seek(int64(rec.offset), io.SeekStart); seekErr != nil {
			f.Close()
			return nil, fmt.Errorf("mdbox/list: seek: %w", seekErr)
		}
		size, parseErr := parseHeader(f)
		f.Close()
		if parseErr != nil {
			return nil, fmt.Errorf("mdbox/list: parse header: %w", parseErr)
		}

		token := fmt.Sprintf("%d:%d", rec.fileID, rec.offset)
		entries = append(entries, entry{
			fileID: rec.fileID,
			offset: rec.offset,
			meta:   &mailbox.MessageMeta{Filename: token, Size: size},
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].fileID != entries[j].fileID {
			return entries[i].fileID < entries[j].fileID
		}
		return entries[i].offset < entries[j].offset
	})

	out := make([]*mailbox.MessageMeta, len(entries))
	for i, e := range entries {
		out[i] = e.meta
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

func (u *userMailbox) ListFolders() ([]string, error) {
	entries, err := os.ReadDir(u.home)
	if err != nil {
		return nil, fmt.Errorf("mdbox/listfolders: %w", err)
	}
	folders := []string{"INBOX"}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "INBOX" || name == "mdbox-storage" {
			continue
		}
		if strings.HasPrefix(name, ".") {
			folders = append(folders, strings.TrimPrefix(name, "."))
		}
	}
	return folders, nil
}

// AppendUIDEntry is a no-op for mdbox — UIDs are managed exclusively by UserIndex.
func (u *userMailbox) AppendUIDEntry(_ string, _ uint32, _ string) error { return nil }

func (u *userMailbox) Close() error { return nil }

// ---- dbox binary format helpers --------------------------------------------

func buildRecord(body []byte, guid string, ts, physSize, virtSize uint32) []byte {
	size := uint64(len(body))
	header := fmt.Sprintf("%sN %08x %016x\n", magicPre, uint32(0), size)
	meta := fmt.Sprintf("%sG%s\nR%08x\nZ%08x\nV%08x\n\n",
		magicPost, guid, ts, physSize, virtSize)

	rec := make([]byte, 0, len(header)+len(body)+len(meta))
	rec = append(rec, header...)
	rec = append(rec, body...)
	rec = append(rec, meta...)
	return rec
}

func parseHeader(r io.Reader) (uint32, error) {
	hdr := make([]byte, 30)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	if hdr[0] != 0x01 || hdr[1] != 0x02 {
		return 0, errors.New("bad dbox magic")
	}
	sz64, err := strconv.ParseUint(strings.TrimSpace(string(hdr[13:29])), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size: %w", err)
	}
	return uint32(sz64), nil
}

// ---- dbox.map helpers ------------------------------------------------------

type mapRecord struct {
	fileID   uint32
	offset   uint32
	size     uint32
	expunged uint8
}

func readMapLines(path string) ([]mapRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []mapRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var rec mapRecord
		var exp int
		if _, err := fmt.Sscanf(line, "%d %d %d %d", &rec.fileID, &rec.offset, &rec.size, &exp); err != nil {
			continue
		}
		rec.expunged = uint8(exp)
		recs = append(recs, rec)
	}
	return recs, sc.Err()
}

func writeMapLines(path string, recs []mapRecord) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, rec := range recs {
		if _, err := fmt.Fprintf(f, "%d %d %d %d\n", rec.fileID, rec.offset, rec.size, rec.expunged); err != nil {
			return err
		}
	}
	return nil
}

// ---- path helpers ----------------------------------------------------------

func (u *userMailbox) folderPath(folder string) string {
	if folder == "INBOX" {
		return filepath.Join(u.home, "INBOX")
	}
	return filepath.Join(u.home, "."+folder)
}

func (u *userMailbox) storageDir() string {
	return filepath.Join(u.home, "mdbox-storage")
}

func (u *userMailbox) mfilePath(storageDir string, fileID uint32) string {
	return filepath.Join(storageDir, fmt.Sprintf("m.%d", fileID))
}

func (u *userMailbox) mapPath(folder string) string {
	return filepath.Join(u.folderPath(folder), "dbox.map")
}

// ---- misc helpers ----------------------------------------------------------

func parseFilename(s string) (fileID, offset uint32, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid mdbox filename %q", s)
	}
	id, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse file_id: %w", err)
	}
	off, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse offset: %w", err)
	}
	return uint32(id), uint32(off), nil
}

func randomGUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%032x", b)
}
