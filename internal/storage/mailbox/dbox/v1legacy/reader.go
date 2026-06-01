// Package v1legacy is a read-only reader for yarilo's
// pre-Phase-3 dbox format. It exists exclusively to feed the
// yarilo-migrate tool: walking a v1 tree, surfacing every
// message body + GUID + InternalDate + sizes, so the migrator
// can re-Save them through the canonical dboxv2 driver.
//
// The v1 on-disk shape:
//
//	<home>/<folder>/u.<seq16hex>
//	  <2 magic bytes>           "\x01\x02"
//	  " N "                     literal ASCII
//	  <8 hex UID slot>          always "00000000"
//	  " "                       single space
//	  <16 hex size>             body byte count
//	  "\n"                      end of header
//	  <body, size bytes>
//	  "\n\x01\x03\n"            magic_post
//	  "G<32 hex>\n"             GUID
//	  "R<8 hex>\n"              received timestamp (unix, hex)
//	  "Z<8 hex>\n"              physical size
//	  "V<8 hex>\n"              virtual size
//	  "\n"                      terminator
//
// Folder layout follows the maildir-style convention: INBOX at
// <home>/INBOX/, every other folder at <home>/.<name>/.
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
	headerLen = 31
	magicPre  = "\x01\x02"
	magicPost = "\n\x01\x03\n"
)

// Message is one decoded v1 message — what the migrator needs to
// re-emit through dboxv2.
type Message struct {
	Filename     string    // "u.<16hex>"
	Body         []byte    // raw bytes, untouched
	Size         uint32    // header-declared body size (== len(Body))
	VSize        uint32    // virtual size from "V" trailer line; 0 when absent
	GUID         [16]byte  // 16-byte GUID from "G" trailer line; zero when absent
	InternalDate time.Time // from "R" trailer; zero when absent
}

// ListFolders returns every folder under home in canonical name
// form (INBOX surfaced as "INBOX"; ".Sent" surfaced as "Sent").
// Folders without a u.* file are still listed — the migrator
// will record them as empty.
func ListFolders(home string) ([]string, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("dbox/v1legacy: list %s: %w", home, err)
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
// Mirrors the legacy driver's layout: INBOX at the home root,
// everything else at <home>/.<name>/. The returned path is not
// verified to exist.
func FolderPath(home, folder string) string {
	if folder == "INBOX" {
		return filepath.Join(home, "INBOX")
	}
	return filepath.Join(home, "."+folder)
}

// ListMessages walks the folder directory and returns every
// "u.<seq>" filename in lexicographic order (which matches v1
// save order, since seq is a 16-hex monotonic counter). Missing
// folder is not an error — returns (nil, nil).
func ListMessages(home, folder string) ([]string, error) {
	dir := FolderPath(home, folder)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dbox/v1legacy: list %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "u.") {
			continue
		}
		names = append(names, name)
	}
	// ReadDir returns lexicographic on most filesystems but the
	// spec doesn't guarantee it — sort explicitly.
	strSort(names)
	return names, nil
}

// ReadMessage decodes one v1 file into a Message. Returns an
// error when the magic or header layout is wrong; corrupt trailer
// fields are tolerated (left zero) so partially-damaged files
// can still be migrated.
func ReadMessage(home, folder, filename string) (*Message, error) {
	p := filepath.Join(FolderPath(home, folder), filename)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("dbox/v1legacy: read %s: %w", p, err)
	}
	return decode(filename, data)
}

func decode(filename string, data []byte) (*Message, error) {
	if len(data) < headerLen {
		return nil, fmt.Errorf("dbox/v1legacy: %s: short (%d bytes < %d)", filename, len(data), headerLen)
	}
	if data[0] != magicPre[0] || data[1] != magicPre[1] {
		return nil, fmt.Errorf("dbox/v1legacy: %s: bad magic", filename)
	}
	if string(data[2:5]) != " N " {
		return nil, fmt.Errorf("dbox/v1legacy: %s: bad header type", filename)
	}
	var physSize uint32
	if _, err := fmt.Sscanf(string(data[14:30]), "%x", &physSize); err != nil {
		return nil, fmt.Errorf("dbox/v1legacy: %s: parse size: %w", filename, err)
	}
	bodyEnd := headerLen + int(physSize)
	if bodyEnd > len(data) {
		return nil, fmt.Errorf("dbox/v1legacy: %s: body out of range", filename)
	}
	body := make([]byte, physSize)
	copy(body, data[headerLen:bodyEnd])
	msg := &Message{
		Filename: filename,
		Body:     body,
		Size:     physSize,
		VSize:    physSize, // identity fallback; trailer V overrides below
	}
	if bodyEnd < len(data) {
		parseTrailer(data[bodyEnd:], msg)
	}
	return msg, nil
}

func parseTrailer(trailer []byte, msg *Message) {
	br := bufio.NewReader(strings.NewReader(string(trailer)))
	// Skip magic_post if present (we tolerate its absence — some
	// historical writes may have skipped it).
	if len(trailer) >= len(magicPost) && string(trailer[:len(magicPost)]) == magicPost {
		_, _ = br.Discard(len(magicPost))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			return
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if err == io.EOF {
				return
			}
			continue
		}
		key := line[0]
		val := strings.TrimSpace(line[1:])
		switch key {
		case 'G':
			if raw, err := hex.DecodeString(val); err == nil && len(raw) == 16 {
				copy(msg.GUID[:], raw)
			}
		case 'R':
			if v, err := strconv.ParseUint(val, 16, 32); err == nil {
				msg.InternalDate = time.Unix(int64(v), 0).UTC()
			}
		case 'V':
			if v, err := strconv.ParseUint(val, 16, 32); err == nil {
				msg.VSize = uint32(v)
			}
		case 'Z':
			// Z is the canonical physical size. We trust the
			// header value when they disagree; Z drift is rare.
			_ = val
		}
		if err == io.EOF {
			return
		}
	}
}

// strSort is the std-library-free in-place sort the package
// uses to avoid pulling sort.* into the migration code path.
// Names are very short (u.<16hex>) so a one-pass insertion
// sort is fine.
func strSort(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}
