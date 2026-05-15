// Package dbox implements MailboxBackend for the sdbox (single-message dbox) format.
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

// Backend is the sdbox MailboxBackend factory.
// It holds only a process-wide sequence counter for unique filenames.
type Backend struct {
	counter atomic.Uint64
}

// New creates an sdbox backend.
func New() *Backend {
	return &Backend{}
}

// OpenUser returns a per-session handle bound to u.
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserMailbox {
	return &userMailbox{b: b, home: u.Home}
}

// userMailbox is a per-session, per-user sdbox storage handle.
type userMailbox struct {
	b    *Backend
	home string
}

func (u *userMailbox) Init() error {
	if err := os.MkdirAll(filepath.Join(u.home, "INBOX"), 0o700); err != nil {
		return fmt.Errorf("dbox/init: %w", err)
	}
	return nil
}

func (u *userMailbox) Create(folder string) error {
	if err := os.MkdirAll(u.folderPath(folder), 0o700); err != nil {
		return fmt.Errorf("dbox/create: %w", err)
	}
	return nil
}

func (u *userMailbox) Delete(folder string) error {
	if err := os.RemoveAll(u.folderPath(folder)); err != nil {
		return fmt.Errorf("dbox/delete: %w", err)
	}
	return nil
}

func (u *userMailbox) Save(folder string, r io.Reader, size int64, flags []string) (string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("dbox/save read: %w", err)
	}
	physSize := uint32(len(body))

	seq := u.b.counter.Add(1)
	filename := fmt.Sprintf("u.%016x", seq)
	dst := filepath.Join(u.folderPath(folder), filename)

	guid := randomGUID()
	now := time.Now()

	var buf bytes.Buffer
	// Header: <magic_pre> N <uid_hex8> <size_hex16>\n  (uid=0; index manages UIDs)
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

	_ = flags // flags managed by index, not embedded in the file
	return filename, nil
}

func (u *userMailbox) Fetch(folder, filename string) (io.ReadCloser, error) {
	p := filepath.Join(u.folderPath(folder), filename)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("dbox/fetch %s: %w", filename, err)
	}

	// Header is always 31 bytes: magicPre(2) + ' N ' (3) + uid8(8) + ' ' (1) + size16(16) + '\n'(1)
	const headerLen = 31
	if len(data) < headerLen {
		return nil, fmt.Errorf("dbox/fetch %s: file too short (%d bytes)", filename, len(data))
	}

	var physSize uint32
	if _, err = fmt.Sscanf(string(data[14:30]), "%x", &physSize); err != nil {
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

func (u *userMailbox) Remove(folder, filename string) error {
	p := filepath.Join(u.folderPath(folder), filename)
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dbox/remove: %w", err)
	}
	return nil
}

func (u *userMailbox) List(folder string) ([]*mailbox.MessageMeta, error) {
	dir := u.folderPath(folder)
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
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	msgs := make([]*mailbox.MessageMeta, len(items))
	for i, it := range items {
		msgs[i] = &mailbox.MessageMeta{
			Filename: it.name,
			UID:      0, // UserIndex assigns UIDs
			Size:     it.size,
		}
	}
	return msgs, nil
}

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	_, err := os.Stat(u.folderPath(folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dbox/folderexists: %w", err)
	}
	return true, nil
}

func (u *userMailbox) ListFolders() ([]string, error) {
	entries, err := os.ReadDir(u.home)
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

// AppendUIDEntry is a no-op for sdbox — UIDs are managed exclusively by UserIndex.
func (u *userMailbox) AppendUIDEntry(_ string, _ uint32, _ string) error { return nil }

func (u *userMailbox) Close() error { return nil }

// ---- path helpers ----------------------------------------------------------

func (u *userMailbox) folderPath(folder string) string {
	if folder == "INBOX" {
		return filepath.Join(u.home, "INBOX")
	}
	return filepath.Join(u.home, "."+folder)
}

func randomGUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%032x", b)
}
