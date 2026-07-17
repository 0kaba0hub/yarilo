package mdbox

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// parsedTrailer carries the per-message metadata recovered from
// a record's metadata trailer.
type parsedTrailer struct {
	guid         [16]byte
	internalDate time.Time
}

// scanTrailer reads a dbox v2 metadata trailer starting at the
// current reader position. Returns the byte count consumed (from
// the metadata magic_post through the terminating blank line)
// plus the parsed fields. limit caps the scan so a missing
// terminator on a corrupt file can't run off the end.
func scanTrailer(r io.Reader, limit uint32) (uint32, parsedTrailer, error) {
	br := bufio.NewReader(io.LimitReader(r, int64(limit)))
	// Magic_post: "\n\x01\x03\n" (4 bytes).
	mp := make([]byte, len(magicPost))
	if _, err := io.ReadFull(br, mp); err != nil {
		return 0, parsedTrailer{}, fmt.Errorf("read magic_post: %w", err)
	}
	if string(mp) != magicPost {
		return 0, parsedTrailer{}, fmt.Errorf("bad magic_post")
	}
	consumed := uint32(len(magicPost))
	var out parsedTrailer
	for {
		line, err := br.ReadString('\n')
		consumed += uint32(len(line))
		if line == "\n" {
			// Blank line — trailer terminator.
			return consumed, out, nil
		}
		if err == io.EOF {
			return consumed, out, nil
		}
		if err != nil {
			return consumed, out, fmt.Errorf("read trailer line: %w", err)
		}
		line = strings.TrimRight(line, "\n")
		if len(line) < 2 {
			continue
		}
		val := strings.TrimSpace(line[1:])
		switch line[0] {
		case 'G':
			if raw, derr := hex.DecodeString(val); derr == nil && len(raw) == 16 {
				copy(out.guid[:], raw)
			}
		case 'R':
			if v, derr := strconv.ParseUint(val, 16, 32); derr == nil {
				out.internalDate = time.Unix(int64(v), 0).UTC()
			}
		}
	}
}

// Scan implements UserMailbox.Scan for the mdbox driver. Walks
// every m.<N> file in <home>/mdbox/storage and yields one
// ScanRecord per stored message. Used by the admin rebuild flow
// when the map index is corrupt and we need to reconstruct
// state from on-disk bytes.
//
// Returned records carry: Filename (= stringified map_uid IF the
// record is still resolvable through the current map, otherwise
// 0), Size (body bytes), GUID + InternalDate (parsed from the
// dbox metadata trailer). The folder argument is ignored because
// mdbox storage is folder-agnostic; the caller must pair the
// scan output with per-folder fileindex records to know which
// folder each map_uid belongs to.
func (u *userMailbox) scanStorage() ([]mailbox.ScanRecord, error) {
	// Collect fileID → on-disk path across BOTH tiers. Primary wins when a file
	// exists in both (a half-finished altmove); an alt-only file is cold-tier
	// mail that a primary-only scan would silently drop.
	paths := map[uint32]string{}
	addDir := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasPrefix(name, "m.") {
				continue
			}
			id64, perr := strconv.ParseUint(strings.TrimPrefix(name, "m."), 10, 32)
			if perr != nil {
				continue
			}
			fid := uint32(id64)
			if _, seen := paths[fid]; seen {
				continue // primary added first — keep it over the alt copy.
			}
			paths[fid] = filepath.Join(dir, name)
		}
		return nil
	}
	if err := addDir(u.storagePath()); err != nil {
		return nil, fmt.Errorf("mdbox/scan: read storage: %w", err)
	}
	if u.AltEnabled() {
		if err := addDir(u.altStoragePath()); err != nil {
			return nil, fmt.Errorf("mdbox/scan: read alt storage: %w", err)
		}
	}

	// Stable order so multi-file scans are deterministic.
	fileIDs := make([]uint32, 0, len(paths))
	for fid := range paths {
		fileIDs = append(fileIDs, fid)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	m, _ := u.openMap() // may be nil if map is unrecoverable; we tolerate that.

	out := make([]mailbox.ScanRecord, 0, len(fileIDs)*4)
	for _, fileID := range fileIDs {
		recs, serr := u.scanMFileAt(paths[fileID])
		if serr != nil {
			// A corrupt record quarantines its file: keep the good prefix and
			// carry on. A hard open/stat error skips the file entirely. Either
			// way one bad m.<N> must not abort the whole storage rebuild.
			if errors.Is(serr, errScanCorrupt) {
				slog.Warn("mdbox/scan: quarantined corrupt m.<N>, keeping good prefix",
					"user", u.username, "file", fileID, "kept", len(recs), "err", serr)
			} else {
				slog.Warn("mdbox/scan: skipping unreadable m.<N>",
					"user", u.username, "file", fileID, "err", serr)
				continue
			}
		}
		if m != nil {
			if mapRecs, err := m.RecordsInFile(fileID); err == nil {
				resolveMapFilenames(recs, mapRecs)
			}
		}
		for _, r := range recs {
			out = append(out, r.scan)
		}
	}
	return out, nil
}

// resolveMapFilenames pairs physical scan records with map entries
// to populate Filename (= stringified map_uid). Two strategies are
// tried in order:
//
//  1. GUID match — the GUID from the dbox trailer is compared
//     against the GUID in the map entry (when the map carries one).
//     Robust against offset shifts from partial file corruption.
//  2. Offset match — fallback for map entries without a stored GUID
//     (records written before GUID indexing was introduced).
//
// Records that match via neither strategy keep an empty Filename;
// the rebuild flow treats them as orphaned and rescans per-folder
// fileindexes.
func resolveMapFilenames(recs []scanRecord, mapEntries []mdboxmap.MapEntry) {
	type guidKey = [16]byte
	guidToUID := make(map[guidKey]uint32, len(mapEntries))
	offsetToUID := make(map[uint32]uint32, len(mapEntries))
	for _, e := range mapEntries {
		if e.GUID != (guidKey{}) {
			guidToUID[e.GUID] = e.UID
		}
		offsetToUID[e.Offset] = e.UID
	}
	for i := range recs {
		if recs[i].scan.Filename != "" {
			continue
		}
		// Strategy 1: GUID match.
		if recs[i].scan.GUID != (guidKey{}) {
			if uid, ok := guidToUID[recs[i].scan.GUID]; ok {
				recs[i].scan.Filename = strconv.FormatUint(uint64(uid), 10)
				continue
			}
		}
		// Strategy 2: offset match.
		if uid, ok := offsetToUID[recs[i].physicalOffset]; ok {
			recs[i].scan.Filename = strconv.FormatUint(uint64(uid), 10)
		}
	}
}

// scanRecord wraps a ScanRecord with its physical offset so
// scanStorage can pair it back to a map_uid after the fact.
type scanRecord struct {
	scan           mailbox.ScanRecord
	physicalOffset uint32
}

// physRecord carries the minimal per-dbox-record info that the alt-
// move scanner needs: byte offset within the file and InternalDate
// from the R trailer field. Used by scanMFileForAlt.
type physRecord struct {
	offset       uint32
	internalDate time.Time
}

// scanMFileForAlt reads the dbox v2 records in the file at path and
// returns one physRecord per message — only the physical offset and
// InternalDate (from the R trailer field). Path may point to either
// a primary or an alt-tier m.<N> file.
func scanMFileForAlt(path string) ([]physRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	total := uint32(st.Size())

	var out []physRecord
	pos := uint32(0)
	for pos < total {
		if _, err := f.Seek(int64(pos), io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek %d: %w", pos, err)
		}
		headerLine := make([]byte, 64)
		n, err := f.Read(headerLine)
		if err != nil {
			return nil, fmt.Errorf("read header line @%d: %w", pos, err)
		}
		lfIdx := -1
		for i := 0; i < n; i++ {
			if headerLine[i] == '\n' {
				lfIdx = i
				break
			}
		}
		if lfIdx < 0 {
			return nil, fmt.Errorf("file header line missing LF @%d", pos)
		}
		bodyStart := pos + uint32(lfIdx) + 1
		if _, err := f.Seek(int64(bodyStart), io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek msg header @%d: %w", bodyStart, err)
		}
		mh := make([]byte, messageHeaderSize)
		if _, err := io.ReadFull(f, mh); err != nil {
			return nil, fmt.Errorf("read msg header @%d: %w", bodyStart, err)
		}
		if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
			return nil, fmt.Errorf("bad magic @%d", bodyStart)
		}
		size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parse size @%d: %w", bodyStart, err)
		}
		bodyEnd := bodyStart + messageHeaderSize + uint32(size)
		if bodyEnd > total {
			return nil, fmt.Errorf("body @%d exceeds file size", bodyStart)
		}
		if _, err := f.Seek(int64(bodyEnd), io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek trailer: %w", err)
		}
		trailerEnd, parsed, err := scanTrailer(f, total-bodyEnd)
		if err != nil {
			return nil, fmt.Errorf("trailer @%d: %w", bodyEnd, err)
		}
		out = append(out, physRecord{offset: pos, internalDate: parsed.internalDate})
		pos = bodyEnd + trailerEnd
	}
	return out, nil
}

// errScanCorrupt marks a per-record parse failure inside an m.<N> file. Unlike
// a hard open/stat error it is recoverable at the scan level: the records parsed
// up to the bad offset are still valid, so scanMFile returns them alongside this
// sentinel and scanStorage quarantines the file (logs + keeps the good prefix)
// instead of aborting the whole storage walk.
var errScanCorrupt = errors.New("mdbox/scan: corrupt record")

// scanMFile walks one m.<N> file from offset 0 to EOF, parsing each canonical
// dbox v2 record and emitting a scanRecord per message. A corrupt record cannot
// be skipped past — its size is what tells us where the next record starts — so
// the walk stops at the first bad record and returns the good prefix together
// with errScanCorrupt (the file is quarantined, not silently truncated to zero).
// A hard open/stat error returns a nil slice and a plain error. path may point
// at a primary or an alt-tier m.<N> file.
func (u *userMailbox) scanMFileAt(path string) ([]scanRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	total := uint32(st.Size())

	var out []scanRecord
	pos := uint32(0)
	for pos < total {
		if _, err := f.Seek(int64(pos), io.SeekStart); err != nil {
			return out, fmt.Errorf("%w: seek %d: %v", errScanCorrupt, pos, err)
		}
		// Parse the file-header line (variable length up to LF).
		headerLine := make([]byte, 64)
		n, err := f.Read(headerLine)
		if err != nil {
			return out, fmt.Errorf("%w: read header line @%d: %v", errScanCorrupt, pos, err)
		}
		lfIdx := -1
		for i := 0; i < n; i++ {
			if headerLine[i] == '\n' {
				lfIdx = i
				break
			}
		}
		if lfIdx < 0 {
			return out, fmt.Errorf("%w: file header line missing LF @%d", errScanCorrupt, pos)
		}
		bodyStart := pos + uint32(lfIdx) + 1
		// Read 32-byte message header.
		if _, err := f.Seek(int64(bodyStart), io.SeekStart); err != nil {
			return out, fmt.Errorf("%w: seek msg header @%d: %v", errScanCorrupt, bodyStart, err)
		}
		mh := make([]byte, messageHeaderSize)
		if _, err := io.ReadFull(f, mh); err != nil {
			return out, fmt.Errorf("%w: read msg header @%d: %v", errScanCorrupt, bodyStart, err)
		}
		if mh[0] != magicPreByte0 || mh[1] != magicPreByte1 {
			return out, fmt.Errorf("%w: bad magic @%d", errScanCorrupt, bodyStart)
		}
		size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
		if err != nil {
			return out, fmt.Errorf("%w: parse size @%d: %v", errScanCorrupt, bodyStart, err)
		}
		// Skip body, parse metadata trailer to recover GUID + R.
		bodyEnd := bodyStart + messageHeaderSize + uint32(size)
		if bodyEnd > total {
			return out, fmt.Errorf("%w: body @%d exceeds file size", errScanCorrupt, bodyStart)
		}
		rec := scanRecord{
			scan: mailbox.ScanRecord{
				Size:  uint32(size),
				VSize: uint32(size),
			},
			physicalOffset: pos,
		}
		// Parse trailer: looks like "\n\x01\x03\nG<hex>\nR<hex>\n...\n\n".
		if _, err := f.Seek(int64(bodyEnd), io.SeekStart); err != nil {
			return out, fmt.Errorf("%w: seek trailer @%d: %v", errScanCorrupt, bodyEnd, err)
		}
		trailerEnd, parsed, err := scanTrailer(f, total-bodyEnd)
		if err != nil {
			return out, fmt.Errorf("%w: trailer @%d: %v", errScanCorrupt, bodyEnd, err)
		}
		rec.scan.GUID = parsed.guid
		rec.scan.InternalDate = parsed.internalDate
		out = append(out, rec)
		pos = bodyEnd + trailerEnd
	}
	return out, nil
}
