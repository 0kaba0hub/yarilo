// Package maildir implements the MailboxBackend for Maildir format.
// Filename: {secs}.M{usecs}P{pid}.{hostname}:2,{flags}
// uidlist: dovecot-uidlist v3
package maildir

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
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Backend is a Maildir MailboxBackend.
type Backend struct {
	root     string
	hostname string
	pid      int
	counter  atomic.Uint64
	mu       sync.Mutex // guards uidlist writes
}

// New creates a Maildir backend rooted at root.
func New(root string) (*Backend, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	return &Backend{
		root:     root,
		hostname: hostname,
		pid:      os.Getpid(),
	}, nil
}

func (b *Backend) Init(user string) error {
	base := b.userRoot(user)
	for _, sub := range []string{"INBOX/cur", "INBOX/new", "INBOX/tmp"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Create(user, folder string) error {
	base := b.folderPath(user, folder)
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Delete(user, folder string) error {
	return os.RemoveAll(b.folderPath(user, folder))
}

func (b *Backend) Save(user, folder string, r io.Reader, size int64, flags []string) (string, error) {
	folderPath := b.folderPath(user, folder)
	now := time.Now()
	seq := b.counter.Add(1)
	basename := fmt.Sprintf("%d.M%dP%d_%d.%s",
		now.Unix(), now.UnixMicro()%1_000_000, b.pid, seq, b.hostname)

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
	// so List() can return both sizes without reading the file body. Virtual
	// size = CRLF-normalised bytes (what POP3 RETR transmits).
	finalName := fmt.Sprintf("%s,S=%d,W=%d:2,%s", basename, sc.phys, sc.phys+sc.lfNoCR, flagStr)
	dstPath := filepath.Join(folderPath, "cur", finalName)
	if err := os.Rename(tmpPath, dstPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("maildir: rename to cur: %w", err)
	}
	return finalName, nil
}

// sizeCounter is an io.Writer that records bytes written and the number of LF
// bytes not preceded by CR (lone LFs that would gain a CR under CRLF
// normalisation). Implements io.Writer so it can be plugged into io.TeeReader.
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

func (b *Backend) Fetch(user, folder, filename string) (io.ReadCloser, error) {
	p := filepath.Join(b.folderPath(user, folder), "cur", filename)
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("maildir: fetch %s: %w", filename, err)
	}
	return f, nil
}

func (b *Backend) Remove(user, folder, filename string) error {
	p := filepath.Join(b.folderPath(user, folder), "cur", filename)
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (b *Backend) List(user, folder string) ([]*mailbox.MessageMeta, error) {
	dir := filepath.Join(b.folderPath(user, folder), "cur")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	uidMap, err := b.readUIDList(user, folder)
	if err != nil {
		return nil, err
	}

	var msgs []*mailbox.MessageMeta
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
		uid := uidMap[name]
		msgs = append(msgs, &mailbox.MessageMeta{
			UID:      uid,
			Flags:    flags,
			Keywords: keywords,
			Size:     sz,
			VSize:    virt,
		})
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].UID < msgs[j].UID
	})
	return msgs, nil
}

func (b *Backend) FolderExists(user, folder string) (bool, error) {
	_, err := os.Stat(b.folderPath(user, folder))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (b *Backend) ListFolders(user string) ([]string, error) {
	base := b.userRoot(user)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	folders := []string{"INBOX"}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "INBOX" {
			continue
		}
		// Dovecot stores sub-folders as ".Name" directories
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			folders = append(folders, strings.TrimPrefix(name, "."))
		}
	}
	return folders, nil
}

// ---- uidlist ------------------------------------------------------------

func (b *Backend) uidListPath(user, folder string) string {
	return filepath.Join(b.folderPath(user, folder), "dovecot-uidlist")
}

// readUIDList returns filename→uid map from dovecot-uidlist v3.
func (b *Backend) readUIDList(user, folder string) (map[string]uint32, error) {
	path := b.uidListPath(user, folder)
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
			// v3 header line
			continue
		}
		// uid [options] :filename  — separator is " :" (space+colon before filename)
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

// AppendUIDEntry adds a new entry to dovecot-uidlist v3.
// Called after Save() to record the uid → filename mapping.
func (b *Backend) AppendUIDEntry(user, folder string, uid uint32, filename string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.uidListPath(user, folder)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write header if file is new
	info, _ := f.Stat()
	if info != nil && info.Size() == 0 {
		fmt.Fprintf(f, "3 V%d N%d G%s\n", uint32(time.Now().Unix()), uid+1, randomGUID())
	}
	_, err = fmt.Fprintf(f, "%d :%s\n", uid, filename)
	return err
}

// ---- helpers ------------------------------------------------------------

func (b *Backend) userRoot(user string) string {
	// user@domain → domain/user  (Dovecot virtual hosting layout)
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
	// Sub-folders stored as .FolderName
	return filepath.Join(base, "."+folder)
}

// encodeFlags converts IMAP flag names to sorted Maildir flag chars.
// Standard mapping: Answered→R, Deleted→T, Draft→D, Flagged→F, Seen→S
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

// parseSizeInfo extracts the Dovecot-convention ,S=N,W=N size annotations
// from the part of a Maildir filename before ":2,<flags>". Missing keys leave
// the corresponding hasX false (callers should stat() the file as a fallback).
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

// decodeFlags parses flags and keywords from a Maildir filename.
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
