// Package dbox implements the MailboxBackend for the sdbox (single-message dbox) format.
// Each message is stored in its own file named u.<16hex_seq>.
// File layout follows the Dovecot dbox wire format documented in INTERNALS.md §8.
package dbox

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

var (
	magicPre  = []byte{0x01, 0x02}
	magicPost = []byte{'\n', 0x01, 0x03, '\n'}
)

// Backend is an sdbox MailboxBackend.
type Backend struct {
	root    string
	counter atomic.Uint64
}

// New creates an sdbox backend rooted at root.
func New(root string) (*Backend, error) {
	return &Backend{root: root}, nil
}

func (b *Backend) Init(user string) error {
	base := b.userRoot(user)
	if err := os.MkdirAll(filepath.Join(base, "INBOX"), 0o700); err != nil {
		return fmt.Errorf("dbox/init: %w", err)
	}
	return nil
}

func (b *Backend) Create(user, folder string) error {
	if err := os.MkdirAll(b.folderPath(user, folder), 0o700); err != nil {
		return fmt.Errorf("dbox/create: %w", err)
	}
	return nil
}

func (b *Backend) Delete(user, folder string) error {
	if err := os.RemoveAll(b.folderPath(user, folder)); err != nil {
		return fmt.Errorf("dbox/delete: %w", err)
	}
	return nil
}

func (b *Backend) Save(user, folder string, r io.Reader, size int64, flags []string) (string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("dbox/save read: %w", err)
	}
	physSize := uint32(len(body))

	seq := b.counter.Add(1)
	filename := fmt.Sprintf("u.%016x", seq)
	dst := filepath.Join(b.folderPath(user, folder), filename)

	guid := randomGUID()
	now := time.Now()

	var buf bytes.Buffer
	// Header line: <magic_pre> ' ' N ' ' <uid_hex8> ' ' <size_hex16> '\n'
	// uid is 0 — FileIndex manages IMAP UIDs; we store 0 in the file.
	buf.Write(magicPre)
	fmt.Fprintf(&buf, " N %08x %016x\n", 0, physSize)
	buf.Write(body)
	buf.Write(magicPost)
	fmt.Fprintf(&buf, "G%s\n", guid)
	fmt.Fprintf(&buf, "R%08x\n", uint32(now.Unix()))
	fmt.Fprintf(&buf, "Z%08x\n", physSize)
	fmt.Fprintf(&buf, "V%08x\n", physSize)
	buf.WriteByte('\n')

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("dbox/save create: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		os.Remove(dst)
		return "", fmt.Errorf("dbox/save write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("dbox/save close: %w", err)
	}

	_ = flags // flags are managed by the index, not embedded in the file
	return filename, nil
}

func (b *Backend) Fetch(user, folder, filename string) (io.ReadCloser, error) {
	p := filepath.Join(b.folderPath(user, folder), filename)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("dbox/fetch %s: %w", filename, err)
	}

	// Header is always 31 bytes: magicPre(2) + ' N ' (3) + uid8(8) + ' ' (1) + size16(16) + '\n'(1) = 31
	const headerLen = 31
	if len(data) < headerLen {
		return nil, fmt.Errorf("dbox/fetch %s: file too short (%d bytes)", filename, len(data))
	}

	var physSize uint32
	// Parse size from header bytes [14:30] (the 16-hex physical-size field).
	_, err = fmt.Sscanf(string(data[14:30]), "%x", &physSize)
	if err != nil {
		return nil, fmt.Errorf("dbox/fetch %s: parse size: %w", filename, err)
	}

	bodyStart := headerLen
	bodyEnd := bodyStart + int(physSize)
	if bodyEnd > len(data) {
		return nil, fmt.Errorf("dbox/fetch %s: body out of range", filename)
	}

	body := make([]byte, physSize)
	copy(body, data[bodyStart:bodyEnd])
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (b *Backend) Remove(user, folder, filename string) error {
	p := filepath.Join(b.folderPath(user, folder), filename)
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dbox/remove: %w", err)
	}
	return nil
}

func (b *Backend) List(user, folder string) ([]*mailbox.MessageMeta, error) {
	dir := b.folderPath(user, folder)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("dbox/list: %w", err)
	}

	type item struct {
		name string
		size uint32
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "u.") {
			continue
		}
		info, _ := e.Info()
		var sz uint32
		if info != nil {
			sz = uint32(info.Size())
		}
		items = append(items, item{name: name, size: sz})
	}
	// u.<hex_seq> filenames sort lexicographically in creation order.
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	msgs := make([]*mailbox.MessageMeta, len(items))
	for i, it := range items {
		msgs[i] = &mailbox.MessageMeta{
			UID:  0, // FileIndex assigns UIDs
			Size: it.size,
		}
	}
	return msgs, nil
}

func (b *Backend) FolderExists(user, folder string) (bool, error) {
	_, err := os.Stat(b.folderPath(user, folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dbox/folderexists: %w", err)
	}
	return true, nil
}

func (b *Backend) ListFolders(user string) ([]string, error) {
	base := b.userRoot(user)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("dbox/listfolders: %w", err)
	}
	folders := []string{"INBOX"}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "INBOX" {
			continue
		}
		if strings.HasPrefix(name, ".") {
			folders = append(folders, strings.TrimPrefix(name, "."))
		}
	}
	return folders, nil
}

// ---- helpers ---------------------------------------------------------------

func (b *Backend) userRoot(user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		domain := user[at+1:]
		local := user[:at]
		return filepath.Join(b.root, domain, local)
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

func randomGUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%032x", b)
}
