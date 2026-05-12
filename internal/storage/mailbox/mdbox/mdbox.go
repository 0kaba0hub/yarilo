// Package mdbox implements the MailboxBackend for the mdbox (multi-message dbox) format.
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

// Backend is an mdbox MailboxBackend.
type Backend struct {
	root          string
	mu            sync.Mutex
	currentFileID uint32
	currentSize   int64
	// rotateThreshold is used only in tests to override the 10 MB limit.
	rotateThreshold int64
}

// New creates an mdbox backend rooted at root.
// It scans existing m.* files to resume from the highest file_id.
func New(root string) (*Backend, error) {
	b := &Backend{root: root, rotateThreshold: rotateAt}

	// Walk all user trees looking for mdbox-storage/m.* to find the max file_id.
	// We use a best-effort scan: failure to read a subdir is non-fatal.
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "m.") {
			return nil
		}
		// Only count files inside an mdbox-storage directory.
		if filepath.Base(filepath.Dir(path)) != "mdbox-storage" {
			return nil
		}
		idStr := d.Name()[2:]
		id64, parseErr := strconv.ParseUint(idStr, 10, 32)
		if parseErr != nil {
			return nil
		}
		id := uint32(id64)
		if id > b.currentFileID {
			b.currentFileID = id
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("mdbox/new: %w", err)
	}
	if b.currentFileID == 0 {
		b.currentFileID = 1
	}

	// currentSize stays 0; Save() will stat the file on first write.
	return b, nil
}

// Init creates the INBOX folder for user.
func (b *Backend) Init(user string) error {
	if err := os.MkdirAll(b.folderPath(user, "INBOX"), 0o700); err != nil {
		return fmt.Errorf("mdbox/init: %w", err)
	}
	return nil
}

// Create creates the named folder directory.
func (b *Backend) Create(user, folder string) error {
	if err := os.MkdirAll(b.folderPath(user, folder), 0o700); err != nil {
		return fmt.Errorf("mdbox/create: %w", err)
	}
	return nil
}

// Delete removes a folder directory (and its dbox.map).
// mdbox-storage is shared and is NOT removed.
func (b *Backend) Delete(user, folder string) error {
	if err := os.RemoveAll(b.folderPath(user, folder)); err != nil {
		return fmt.Errorf("mdbox/delete: %w", err)
	}
	return nil
}

// Save writes a message into the mdbox storage and appends a map entry.
// Returns "<file_id>:<offset>" as the filename token.
func (b *Backend) Save(user, folder string, r io.Reader, _ int64, _ []string) (string, error) {
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

	storageDir := b.storageDir(user)
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir storage: %w", err)
	}

	if err := os.MkdirAll(b.folderPath(user, folder), 0o700); err != nil {
		return "", fmt.Errorf("mdbox/save: mkdir folder: %w", err)
	}

	b.mu.Lock()
	// Lazy-initialise currentSize from the actual file on first write.
	if b.currentSize == 0 {
		fi, statErr := os.Stat(b.mfilePath(storageDir, b.currentFileID))
		if statErr == nil {
			b.currentSize = fi.Size()
		}
	}

	if b.currentSize > b.rotateThreshold {
		b.currentFileID++
		b.currentSize = 0
	}

	fileID := b.currentFileID
	offset := b.currentSize
	b.currentSize += recLen
	b.mu.Unlock()

	mpath := b.mfilePath(storageDir, fileID)
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

	mapLine := fmt.Sprintf("%d %d %d 0\n", fileID, offset, physSize)
	mapPath := b.mapPath(user, folder)
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

// Fetch opens the message identified by "<file_id>:<offset>" and returns a reader
// positioned at the start of the raw RFC 5322 body.
func (b *Backend) Fetch(user, folder, filename string) (io.ReadCloser, error) {
	fileID, offset, err := parseFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("mdbox/fetch: %w", err)
	}

	storageDir := b.storageDir(user)
	mpath := b.mfilePath(storageDir, fileID)
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
func (b *Backend) Remove(user, folder, filename string) error {
	fileID, offset, err := parseFilename(filename)
	if err != nil {
		return fmt.Errorf("mdbox/remove: %w", err)
	}

	mapPath := b.mapPath(user, folder)
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

// List returns all non-expunged messages in the folder.
func (b *Backend) List(user, folder string) ([]*mailbox.MessageMeta, error) {
	mapPath := b.mapPath(user, folder)
	lines, err := readMapLines(mapPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mdbox/list: %w", err)
	}

	storageDir := b.storageDir(user)

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

		mpath := b.mfilePath(storageDir, rec.fileID)
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

		entries = append(entries, entry{
			fileID: rec.fileID,
			offset: rec.offset,
			meta:   &mailbox.MessageMeta{Size: size},
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

// FolderExists reports whether the folder directory exists.
func (b *Backend) FolderExists(user, folder string) (bool, error) {
	_, err := os.Stat(b.folderPath(user, folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ListFolders returns INBOX plus any dot-prefixed directories under the user root.
func (b *Backend) ListFolders(user string) ([]string, error) {
	base := b.userRoot(user)
	entries, err := os.ReadDir(base)
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

// ---- dbox binary format helpers -----------------------------------------

// buildRecord assembles the full dbox record bytes for one message.
func buildRecord(body []byte, guid string, ts, physSize, virtSize uint32) []byte {
	size := uint64(len(body))
	// Header: \x01\x02 N <uid_hex8> <size_hex16>\n  (uid is always 0 here; index owns UIDs)
	header := fmt.Sprintf("%sN %08x %016x\n", magicPre, uint32(0), size)
	// Metadata block after body
	meta := fmt.Sprintf("%sG%s\nR%08x\nZ%08x\nV%08x\n\n",
		magicPost, guid, ts, physSize, virtSize)

	rec := make([]byte, 0, len(header)+len(body)+len(meta))
	rec = append(rec, header...)
	rec = append(rec, body...)
	rec = append(rec, meta...)
	return rec
}

// parseHeader reads a dbox record header from r and returns the body size.
// Format: \x01\x02 N <uid_hex8> <size_hex16>\n  (30 bytes total)
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

// ---- dbox.map helpers ---------------------------------------------------

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

// ---- path helpers -------------------------------------------------------

func (b *Backend) userRoot(user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(b.root, user[at+1:], user[:at])
	}
	return filepath.Join(b.root, user)
}

func (b *Backend) folderPath(user, folder string) string {
	base := b.userRoot(user)
	if folder == "INBOX" {
		return filepath.Join(base, "INBOX")
	}
	return filepath.Join(base, "."+folder)
}

func (b *Backend) storageDir(user string) string {
	return filepath.Join(b.userRoot(user), "mdbox-storage")
}

func (b *Backend) mfilePath(storageDir string, fileID uint32) string {
	return filepath.Join(storageDir, fmt.Sprintf("m.%d", fileID))
}

func (b *Backend) mapPath(user, folder string) string {
	return filepath.Join(b.folderPath(user, folder), "dbox.map")
}

// ---- misc helpers -------------------------------------------------------

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
