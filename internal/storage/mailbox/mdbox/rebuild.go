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

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// parsedTrailer carries the per-message metadata recovered from a record's
// metadata trailer.
type parsedTrailer struct {
	guid         [16]byte
	internalDate time.Time
	origMailbox  string
	// vsize is V: the size the message occupies once every line ends CRLF.
	// Zero when the trailer carries no V, which a record this server wrote
	// always does.
	vsize uint32
}

// scanTrailer reads a metadata trailer and returns the bytes consumed with the
// parsed fields. limit keeps a missing terminator from running off the end.
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
		case 'V':
			if v, derr := strconv.ParseUint(val, 16, 32); derr == nil {
				out.vsize = uint32(v)
			}
		case metaOrigMailbox:
			// Verbatim, not space-trimmed: a folder name may contain spaces.
			out.origMailbox = line[1:]
		}
	}
}

// scanStorage yields one ScanRecord per stored message across every m.<N> file.
// Storage knows no folders: the caller pairs the output with per-folder index
// records to learn which folder owns each map_uid.
func (u *userMailbox) scanStorage() ([]mailbox.ScanRecord, error) {
	// Collect fileID -> on-disk path across both tiers. Primary wins when a file
	// exists in both (a half-finished altmove); an alt-only file is cold-tier
	// mail that a primary-only scan would drop.
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
				continue // primary added first; keep it over the alt copy
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

	m, _ := u.openMap() // may be nil if the map is unrecoverable; tolerated

	out := make([]mailbox.ScanRecord, 0, len(fileIDs)*4)
	var firstErr error // first per-file fault; scan is incomplete once this is set
	for _, fileID := range fileIDs {
		recs, serr := u.scanMFileAt(paths[fileID])
		// Keep the good prefix; the scan is now incomplete, and surfaced so a
		// destructive consumer aborts instead of expunging what it could not read.
		if serr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("m.%d: %w", fileID, serr)
			}
			if errors.Is(serr, errScanCorrupt) {
				slog.Warn("mdbox/scan: quarantined corrupt m.<N>, keeping good prefix",
					"user", u.username, "file", fileID, "kept", len(recs), "err", serr)
			} else {
				slog.Warn("mdbox/scan: unreadable m.<N> (transient I/O), scan is incomplete",
					"user", u.username, "file", fileID, "err", serr)
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
	if firstErr != nil {
		// Partial records are returned for a partial-aware consumer, but the error
		// makes idxrebuild.RebuildFolder/ExpungeMissing abort rather than treat the
		// unread messages as expunged.
		return out, fmt.Errorf("%w: %w", ErrScanIncomplete, firstErr)
	}
	return out, nil
}

// resolveMapFilenames pairs scan records with map entries: by GUID first, which
// survives an offset shift from partial corruption, then by offset for entries
// written before GUID indexing. A record matching neither is an orphan.
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

// scanRecord wraps a ScanRecord with its physical offset so scanStorage can
// pair it back to a map_uid after the fact.
type scanRecord struct {
	scan           mailbox.ScanRecord
	physicalOffset uint32
}

// physRecord carries the minimal per-record info the alt-move scanner needs:
// byte offset within the file and InternalDate from the R trailer field.
type physRecord struct {
	offset       uint32
	internalDate time.Time
}

// scanMFileForAlt reads the dbox v2 records in the file at path and returns one
// physRecord per message (physical offset and InternalDate only). path may point
// to a primary or an alt-tier m.<N> file.
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
		window := make([]byte, 64)
		n, err := f.Read(window)
		if err != nil {
			return nil, fmt.Errorf("read record start @%d: %w", pos, err)
		}
		skip, ok := peekFileHeaderLen(window[:n])
		if !ok {
			return nil, fmt.Errorf("malformed record @%d", pos)
		}
		bodyStart := pos + uint32(skip)
		// By M, like every other reader: a store written elsewhere, or by a
		// build from before #1522, announces its own header size, and a scan
		// that assumed ours would misplace every body in the file.
		hdrSize, herr := recordHeaderSize(f, window[:n], skip)
		if herr != nil {
			return nil, fmt.Errorf("header size @%d: %w", bodyStart, herr)
		}
		if _, err := f.Seek(int64(bodyStart), io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek msg header @%d: %w", bodyStart, err)
		}
		mh := make([]byte, hdrSize)
		if _, err := io.ReadFull(f, mh); err != nil {
			return nil, fmt.Errorf("read msg header @%d: %w", bodyStart, err)
		}
		if herr := checkMessageHeader(mh); herr != nil {
			// Written at the other size, which the read path has recovered
			// since #1526: one such record used to stop the scan for good.
			recovered, rerr := readMessageHeaderAtOtherSize(f, int64(bodyStart), hdrSize)
			if rerr != nil {
				return nil, fmt.Errorf("@%d: %w", bodyStart, herr)
			}
			logOtherHeaderSize(f.Name(), bodyStart, hdrSize, len(recovered))
			mh, hdrSize = recovered, len(recovered)
		}
		size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parse size @%d: %w", bodyStart, err)
		}
		bodyEnd := bodyStart + uint32(hdrSize) + uint32(size)
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

// Scan sentinels: errScanCorrupt is unreadable bytes, errScanIO is bytes that
// may be fine and unreadable now -- taken as "vanished" it deletes live mail.
// Both surface as ErrScanIncomplete so an expunging consumer aborts.
var (
	errScanCorrupt = errors.New("mdbox/scan: corrupt record")
	errScanIO      = errors.New("mdbox/scan: I/O error")

	// ErrScanIncomplete: not every stored message was enumerated. A consumer
	// that expunges what the scan did not list must abort, or it deletes live
	// mail it merely failed to read.
	ErrScanIncomplete = errors.New("mdbox/scan: incomplete scan")
)

// scanReadErr classifies a seek/read failure: a truncated read
// (io.EOF/ErrUnexpectedEOF) is structural corruption; anything else (EIO,
// ESTALE) is transient I/O. The cause is preserved in the chain.
func scanReadErr(cause error, where string) error {
	if errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %s: %w", errScanCorrupt, where, cause)
	}
	return fmt.Errorf("%w: %s: %w", errScanIO, where, cause)
}

// scanMFileAt walks one m.<N> file. A corrupt record cannot be skipped past --
// its size is what says where the next one starts -- so the walk stops there and
// returns the good prefix with the classifying error.
func (u *userMailbox) scanMFileAt(path string) ([]scanRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %w", errScanIO, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat: %w", errScanIO, err)
	}
	total := uint32(st.Size())

	var out []scanRecord
	pos := uint32(0)
	for pos < total {
		if _, err := f.Seek(int64(pos), io.SeekStart); err != nil {
			return out, scanReadErr(err, fmt.Sprintf("seek %d", pos))
		}
		// Skip the file-header line only when present (the file's first record, or
		// every record in a legacy per-record-header file).
		window := make([]byte, 64)
		n, err := f.Read(window)
		if err != nil {
			return out, scanReadErr(err, fmt.Sprintf("read record start @%d", pos))
		}
		skip, ok := peekFileHeaderLen(window[:n])
		if !ok {
			return out, fmt.Errorf("%w: malformed record @%d", errScanCorrupt, pos)
		}
		bodyStart := pos + uint32(skip)
		// The header's size comes from M, not from a constant: a store written
		// elsewhere, or by a build from before #1522, announces its own, and a
		// scan that assumed ours would misplace every body in the file.
		hdrSize, herr := recordHeaderSize(f, window[:n], skip)
		if herr != nil {
			return out, fmt.Errorf("%w: header size @%d: %w", errScanCorrupt, bodyStart, herr)
		}
		if _, err := f.Seek(int64(bodyStart), io.SeekStart); err != nil {
			return out, scanReadErr(err, fmt.Sprintf("seek msg header @%d", bodyStart))
		}
		mh := make([]byte, hdrSize)
		if _, err := io.ReadFull(f, mh); err != nil {
			return out, scanReadErr(err, fmt.Sprintf("read msg header @%d", bodyStart))
		}
		if herr := checkMessageHeader(mh); herr != nil {
			recovered, rerr := readMessageHeaderAtOtherSize(f, int64(bodyStart), hdrSize)
			if rerr != nil {
				return out, fmt.Errorf("%w: @%d: %w", errScanCorrupt, bodyStart, herr)
			}
			logOtherHeaderSize(f.Name(), bodyStart, hdrSize, len(recovered))
			mh, hdrSize = recovered, len(recovered)
		}
		size, err := strconv.ParseUint(strings.TrimSpace(string(mh[13:29])), 16, 64)
		if err != nil {
			return out, fmt.Errorf("%w: parse size @%d: %w", errScanCorrupt, bodyStart, err)
		}
		// Skip the body, parse the metadata trailer to recover GUID + R.
		bodyEnd := bodyStart + uint32(hdrSize) + uint32(size)
		if bodyEnd > total {
			return out, fmt.Errorf("%w: body @%d exceeds file size", errScanCorrupt, bodyStart)
		}
		rec := scanRecord{
			scan: mailbox.ScanRecord{
				Size: uint32(size),
				// VSize comes from the trailer's V, not the physical size: a
				// bare-LF record goes out as CRLF (#1527).
				VSize: uint32(size),
			},
			physicalOffset: pos,
		}
		// Trailer looks like "\n\x01\x03\nG<hex>\nR<hex>\n...\n\n". A broken trailer
		// is structural: record framing is lost from here on.
		if _, err := f.Seek(int64(bodyEnd), io.SeekStart); err != nil {
			return out, scanReadErr(err, fmt.Sprintf("seek trailer @%d", bodyEnd))
		}
		trailerEnd, parsed, err := scanTrailer(f, total-bodyEnd)
		if err != nil {
			return out, fmt.Errorf("%w: trailer @%d: %w", errScanCorrupt, bodyEnd, err)
		}
		rec.scan.GUID = parsed.guid
		rec.scan.InternalDate = parsed.internalDate
		rec.scan.OrigMailbox = parsed.origMailbox
		if parsed.vsize > 0 {
			rec.scan.VSize = parsed.vsize
		}
		out = append(out, rec)
		pos = bodyEnd + trailerEnd
	}
	return out, nil
}
