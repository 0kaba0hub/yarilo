// Package v1legacy is a read-only reader for yarilo's
// pre-Phase-5 mdbox format. It exists exclusively to feed the
// yarilo-migrate tool: walking a v1 mdbox tree, surfacing every
// message body so the migrator can re-Save them through the
// canonical Phase-5+ mdbox driver.
//
// The v1 on-disk shape:
//
//	<home>/mdbox-storage/m.<file_id>      ← multi-message body files
//	<home>/INBOX/dbox.map                  ← TSV per-folder map
//	<home>/.<folder>/dbox.map              ← same for non-INBOX
//
// Each dbox.map line is `<file_id> <offset> <size> <expunged>`.
// Each entry in m.<file_id> at offset has the dbox single-message
// shape: 30-byte ASCII header + body + "\n\x01\x03\n" metadata
// trailer (G/R/Z/V key/value lines + blank).
package v1legacy

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	headerLen = 30
	magicPre  = "\x01\x02"
	magicPost = "\n\x01\x03\n"
)

// Message is one decoded v1 mdbox message — what the migrator
// needs to re-emit through the new driver.
type Message struct {
	FileID       uint32
	Offset       uint32
	Size         uint32
	Body         []byte
	GUID         [16]byte
	InternalDate time.Time
}

// ListFolders returns every folder under home in canonical name
// form (INBOX surfaced as "INBOX"; ".Sent" surfaced as "Sent").
// Folders without a dbox.map are still listed — the migrator
// records them as empty.
func ListFolders(home string) ([]string, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: list %s: %w", home, err)
	}
	folders := []string{}
	hasInbox := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case name == "INBOX":
			hasInbox = true
		case name == "mdbox-storage":
			continue
		case strings.HasPrefix(name, "."):
			folders = append(folders, strings.TrimPrefix(name, "."))
		}
	}
	if hasInbox {
		folders = append([]string{"INBOX"}, folders...)
	}
	return folders, nil
}

// FolderPath resolves the on-disk directory backing folder.
func FolderPath(home, folder string) string {
	if folder == "INBOX" {
		return filepath.Join(home, "INBOX")
	}
	return filepath.Join(home, "."+folder)
}

// MapPath returns the dbox.map sidecar for folder.
func MapPath(home, folder string) string {
	return filepath.Join(FolderPath(home, folder), "dbox.map")
}

// StorageDir returns the user-wide multi-message storage root.
func StorageDir(home string) string { return filepath.Join(home, "mdbox-storage") }

// MapEntry is one parsed line from a folder's dbox.map.
type MapEntry struct {
	FileID   uint32
	Offset   uint32
	Size     uint32
	Expunged bool
}

// ReadMap returns every entry from a folder's dbox.map sidecar.
// Missing folder is not an error — returns (nil, nil).
func ReadMap(home, folder string) ([]MapEntry, error) {
	f, err := os.Open(MapPath(home, folder))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: open map %s: %w", folder, err)
	}
	defer f.Close()
	var out []MapEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e MapEntry
		var exp int
		if _, err := fmt.Sscanf(line, "%d %d %d %d", &e.FileID, &e.Offset, &e.Size, &exp); err != nil {
			continue
		}
		e.Expunged = exp != 0
		out = append(out, e)
	}
	return out, sc.Err()
}

// ReadMessage decodes one v1 message at entry (file_id, offset).
// Returns an error on bad magic / short read; corrupt trailer
// fields are tolerated (left zero) so partially-damaged files
// can still be migrated.
func ReadMessage(home string, entry MapEntry) (*Message, error) {
	mpath := filepath.Join(StorageDir(home), fmt.Sprintf("m.%d", entry.FileID))
	f, err := os.Open(mpath)
	if err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: open m.%d: %w", entry.FileID, err)
	}
	defer f.Close()
	if _, err := f.Seek(int64(entry.Offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: seek: %w", err)
	}
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: read header: %w", err)
	}
	if hdr[0] != 0x01 || hdr[1] != 0x02 {
		return nil, fmt.Errorf("mdbox/v1legacy: bad magic at m.%d:%d", entry.FileID, entry.Offset)
	}
	sz, err := strconv.ParseUint(strings.TrimSpace(string(hdr[13:29])), 16, 64)
	if err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: parse size: %w", err)
	}
	body := make([]byte, sz)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("mdbox/v1legacy: read body: %w", err)
	}
	msg := &Message{
		FileID: entry.FileID,
		Offset: entry.Offset,
		Size:   uint32(sz),
		Body:   body,
	}
	// Best-effort trailer parse: stop at blank line.
	br := bufio.NewReader(f)
	// Consume magic_post if present.
	post := make([]byte, len(magicPost))
	if _, err := io.ReadFull(br, post); err == nil && string(post) != magicPost {
		// Not magic_post — bail out, trailer absent.
		return msg, nil
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			return msg, nil
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			return msg, nil
		}
		if len(line) < 2 {
			continue
		}
		val := strings.TrimSpace(line[1:])
		switch line[0] {
		case 'G':
			if raw, derr := hex.DecodeString(val); derr == nil && len(raw) == 16 {
				copy(msg.GUID[:], raw)
			}
		case 'R':
			if v, derr := strconv.ParseUint(val, 16, 32); derr == nil {
				msg.InternalDate = time.Unix(int64(v), 0).UTC()
			}
		}
		if err == io.EOF {
			return msg, nil
		}
	}
}
