package mdbox

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// brokenSuffix names the copy of the original kept beside a rewritten file, as
// the reference does: the bytes a rewrite dropped stay inspectable.
const brokenSuffix = ".broken"

// normalisingSuffix names the in-progress copy. A leftover is a crashed rewrite:
// it is removed, never normalised as if it were storage.
const normalisingSuffix = ".normalising"

// normaliseFrames rewrites path when a record is framed at a size the file does
// not announce: good records re-framed, original kept as .broken (#1687).
func normaliseFrames(path string) ([]placed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("mdbox/normalise: open: %w", err)
	}
	defer f.Close() //nolint:errcheck
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("mdbox/normalise: stat: %w", err)
	}
	total := uint32(st.Size())

	head, records, mismatch, err := readFrames(f, total)
	if err != nil {
		return nil, err
	}
	if !mismatch {
		return nil, nil
	}
	// A .broken already there is some earlier original: overwriting it loses
	// what this rewrite replaces, so refuse and name it.
	broken := path + brokenSuffix
	if _, serr := os.Stat(broken); serr == nil {
		return nil, fmt.Errorf("mdbox/normalise: %s already exists; move it aside and re-run", broken)
	}
	return replaceWithNormalised(path, head, records)
}

// frame is one record's bytes: the header carries the size to re-emit, the rest
// is copied verbatim, and the GUID is what repoints the map.
type frame struct {
	size           uint64
	hdrSize        int
	guid           [16]byte
	bodyAndTrailer []byte
}

// placed is where a record ended up after the rewrite, for the map.
type placed struct {
	guid   [16]byte
	offset uint32
	size   uint32
}

// readFrames walks the records, stopping at the first one no header size
// explains -- the reference writes what it read up to there and no further.
func readFrames(f *os.File, total uint32) (head []byte, out []frame, mismatch bool, err error) {
	pos := uint32(0)
	announced := 0
	for pos < total {
		if _, serr := f.Seek(int64(pos), io.SeekStart); serr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: seek %d: %w", pos, serr)
		}
		window := make([]byte, 64)
		n, rerr := f.Read(window)
		if rerr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: read @%d: %w", pos, rerr)
		}
		skip, ok := peekFileHeaderLen(window[:n])
		if !ok {
			break
		}
		if skip > 0 {
			head = append(head, window[:skip]...)
		}
		hdrSize, herr := recordHeaderSize(f, window[:n], skip)
		if herr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: header size @%d: %w", pos, herr)
		}
		announced = hdrSize
		hdrOff := int64(pos) + int64(skip)
		mh := make([]byte, hdrSize)
		if _, rerr := f.ReadAt(mh, hdrOff); rerr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: read header @%d: %w", hdrOff, rerr)
		}
		actual := hdrSize
		if cerr := checkMessageHeader(mh); cerr != nil {
			recovered, oerr := readMessageHeaderAtOtherSize(f, hdrOff, hdrSize)
			if oerr != nil {
				break // no size explains it: keep what was read, as the reference does
			}
			mh, actual = recovered, len(recovered)
			mismatch = true
		}
		size, perr := parseHeaderSize(mh)
		if perr != nil {
			break
		}
		bodyEnd := hdrOff + int64(actual) + int64(size)
		if bodyEnd > int64(total) {
			break
		}
		if _, serr := f.Seek(bodyEnd, io.SeekStart); serr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: seek trailer: %w", serr)
		}
		trailerEnd, _, terr := scanTrailer(f, total-uint32(bodyEnd))
		if terr != nil {
			break
		}
		rest := make([]byte, int64(size)+int64(trailerEnd))
		if _, rerr := f.ReadAt(rest, hdrOff+int64(actual)); rerr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: read body @%d: %w", hdrOff, rerr)
		}
		if _, serr := f.Seek(bodyEnd, io.SeekStart); serr != nil {
			return nil, nil, false, fmt.Errorf("mdbox/normalise: seek trailer: %w", serr)
		}
		_, parsed, terr2 := scanTrailer(f, total-uint32(bodyEnd))
		if terr2 != nil {
			break
		}
		out = append(out, frame{size: size, hdrSize: announced, guid: parsed.guid, bodyAndTrailer: rest})
		pos = uint32(bodyEnd) + trailerEnd
	}
	return head, out, mismatch, nil
}

// replaceWithNormalised writes the frames into a sibling temp file, keeps the
// original as .broken and renames over it, returning where each record landed.
func replaceWithNormalised(path string, head []byte, frames []frame) ([]placed, error) {
	tmp := path + normalisingSuffix
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mdbox/normalise: create %s: %w", tmp, err)
	}
	var landed []placed
	writeErr := func() error {
		at := uint32(0)
		if len(head) > 0 {
			if _, werr := out.Write(head); werr != nil {
				return werr
			}
			at += uint32(len(head))
		}
		for _, fr := range frames {
			landed = append(landed, placed{guid: fr.guid, offset: at, size: uint32(fr.size)})
			hdr := buildMessageHeader(fr.size, fr.hdrSize)
			if _, werr := out.Write(hdr); werr != nil {
				return werr
			}
			if _, werr := out.Write(fr.bodyAndTrailer); werr != nil {
				return werr
			}
			at += uint32(len(hdr) + len(fr.bodyAndTrailer))
		}
		return out.Sync()
	}()
	if cerr := out.Close(); cerr != nil && writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("mdbox/normalise: write %s: %w", tmp, writeErr)
	}
	broken := path + brokenSuffix
	if lerr := os.Link(path, broken); lerr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("mdbox/normalise: keep %s: %w", broken, lerr)
	}
	if len(frames) == 0 {
		// Nothing readable survived. The reference deletes such a file rather
		// than keeping an empty one; the .broken copy holds what there was.
		_ = os.Remove(tmp)
		if rerr := os.Remove(path); rerr != nil {
			return nil, fmt.Errorf("mdbox/normalise: remove empty %s: %w", path, rerr)
		}
		slog.Warn("mdbox: rewritten file had no readable records; removed, original kept",
			"file", filepath.Base(path), "broken_copy", filepath.Base(broken))
		return nil, nil
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		return nil, fmt.Errorf("mdbox/normalise: replace %s: %w", path, rerr)
	}
	slog.Warn("mdbox: rewrote records the file's announced header size does not describe; "+
		"copy of the original kept",
		"file", filepath.Base(path), "broken_copy", filepath.Base(broken), "records", len(frames))
	return landed, nil
}

// buildMessageHeader emits the header at hdrSize: magic, 'N', spaces, the hex
// size at 13..29, LF last. One builder, so a rewrite and a save agree.
func buildMessageHeader(size uint64, hdrSize int) []byte {
	hdr := make([]byte, hdrSize)
	for i := range hdr {
		hdr[i] = ' '
	}
	hdr[0] = magicPreByte0
	hdr[1] = magicPreByte1
	hdr[2] = 'N'
	copy(hdr[13:29], fmt.Sprintf("%016x", size))
	hdr[hdrSize-1] = '\n'
	return hdr
}

// parseHeaderSize reads the size field a message header announces.
func parseHeaderSize(mh []byte) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
}

// normaliseStorageFrames rewrites every m.<N> holding a record at the other
// size. One pass per file per rebuild, as the reference bounds it (#1687).
func (u *userMailbox) normaliseStorageFrames() (int, error) {
	countDirReads.Add(1)
	entries, err := os.ReadDir(u.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("mdbox/normalise: list storage: %w", err)
	}
	m, err := u.openMap()
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(u.storagePath(), name)
		if strings.HasSuffix(name, normalisingSuffix) {
			// A crashed rewrite. Removing it is safe: the original is still in
			// place, and normalising it as storage would be nonsense.
			if rerr := os.Remove(path); rerr != nil {
				return fixed, fmt.Errorf("mdbox/normalise: remove leftover %s: %w", path, rerr)
			}
			continue
		}
		fileID, ok := parseMFileName(name)
		if !ok {
			continue
		}
		landed, nerr := normaliseFrames(path)
		if nerr != nil {
			return fixed, nerr
		}
		if landed == nil {
			continue
		}
		if rerr := repointMap(m, fileID, landed); rerr != nil {
			return fixed, rerr
		}
		fixed++
	}
	return fixed, nil
}

// parseMFileName accepts m.<N> and nothing else, so a .broken copy or any other
// sibling is never read as storage.
func parseMFileName(name string) (uint32, bool) {
	rest, ok := strings.CutPrefix(name, "m.")
	if !ok || rest == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(id), true
}

// repointMap moves every record of the rewritten file to where it now is: the
// map holds byte offsets, and a re-framed record shifts what follows.
func repointMap(m *mdboxmap.Map, fileID uint32, landed []placed) error {
	entries, err := m.RecordsInFile(fileID)
	if err != nil {
		return fmt.Errorf("mdbox/normalise: records in m.%d: %w", fileID, err)
	}
	byGUID := make(map[[16]byte]uint32, len(entries))
	for _, e := range entries {
		if e.GUID != ([16]byte{}) {
			byGUID[e.GUID] = e.UID
		}
	}
	var moved []mdboxmap.MovedRecord
	for _, p := range landed {
		uid, ok := byGUID[p.guid]
		if !ok {
			// Not in the map: an orphan the rebuild deals with by refcount, and
			// nothing points at its offset.
			continue
		}
		moved = append(moved, mdboxmap.MovedRecord{
			UID: uid, FileID: fileID, Offset: p.offset, Size: p.size, GUID: p.guid,
		})
	}
	if len(moved) == 0 {
		return nil
	}
	if aerr := m.AppendMove(moved, nil); aerr != nil {
		return fmt.Errorf("mdbox/normalise: repoint m.%d: %w", fileID, aerr)
	}
	return nil
}
