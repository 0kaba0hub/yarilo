package mdbox

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// brokenSuffix names the copy of the original kept beside a rewritten file, as
// the reference does: the bytes a rewrite dropped stay inspectable.
const brokenSuffix = ".broken"

// normaliseFrames rewrites path when a record is framed at a size the file does
// not announce: good records re-framed, original kept as .broken (#1687).
func normaliseFrames(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("mdbox/normalise: open: %w", err)
	}
	defer f.Close() //nolint:errcheck
	st, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("mdbox/normalise: stat: %w", err)
	}
	total := uint32(st.Size())

	head, records, mismatch, err := readFrames(f, total)
	if err != nil {
		return false, err
	}
	if !mismatch {
		return false, nil
	}
	return true, replaceWithNormalised(path, head, records)
}

// frame is one record's bytes, split where the rewrite needs them: the header
// carries the size to re-emit, the rest is copied verbatim.
type frame struct {
	size           uint64
	hdrSize        int
	bodyAndTrailer []byte
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
		out = append(out, frame{size: size, hdrSize: announced, bodyAndTrailer: rest})
		pos = uint32(bodyEnd) + trailerEnd
	}
	return head, out, mismatch, nil
}

// replaceWithNormalised writes the frames at the announced size into a sibling
// temp file, links the original aside as .broken, and renames over it.
func replaceWithNormalised(path string, head []byte, frames []frame) error {
	tmp := path + ".normalising"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("mdbox/normalise: create %s: %w", tmp, err)
	}
	writeErr := func() error {
		if len(head) > 0 {
			if _, werr := out.Write(head); werr != nil {
				return werr
			}
		}
		for _, fr := range frames {
			hdr := buildMessageHeader(fr.size, fr.hdrSize)
			if _, werr := out.Write(hdr); werr != nil {
				return werr
			}
			if _, werr := out.Write(fr.bodyAndTrailer); werr != nil {
				return werr
			}
		}
		return out.Sync()
	}()
	if cerr := out.Close(); cerr != nil && writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mdbox/normalise: write %s: %w", tmp, writeErr)
	}
	broken := path + brokenSuffix
	if lerr := os.Link(path, broken); lerr != nil && !os.IsExist(lerr) {
		_ = os.Remove(tmp)
		return fmt.Errorf("mdbox/normalise: keep %s: %w", broken, lerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		return fmt.Errorf("mdbox/normalise: replace %s: %w", path, rerr)
	}
	slog.Warn("mdbox: rewrote records the file's announced header size does not describe; "+
		"copy of the original kept",
		"file", filepath.Base(path), "broken_copy", filepath.Base(broken), "records", len(frames))
	return nil
}

// buildMessageHeader emits the reference message header at hdrSize: magic, 'N',
// spaces, the 16-char hex size at 13..29, LF last.
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
	entries, err := os.ReadDir(u.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("mdbox/normalise: list storage: %w", err)
	}
	fixed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "m.") || strings.Contains(e.Name(), ".") && strings.HasSuffix(e.Name(), brokenSuffix) {
			continue
		}
		path := filepath.Join(u.storagePath(), e.Name())
		rewrote, nerr := normaliseFrames(path)
		if nerr != nil {
			return fixed, nerr
		}
		if rewrote {
			fixed++
		}
	}
	return fixed, nil
}
