package mdbox

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// PurgeStats reports what a single Purge invocation accomplished.
type PurgeStats struct {
	FilesScanned    int   // m.<N> files visited
	FilesRewritten  int   // m.<N> files compacted into a fresh id
	FilesUnlinked   int   // m.<N> files removed entirely (all-zero-ref case)
	RecordsKept     int   // map records with refcount > 0 moved forward
	RecordsExpunged int   // map records with refcount == 0 dropped
	BytesReclaimed  int64 // total bytes freed from the storage tree
}

// Purge walks every m.<N> file holding a zero-ref record:
//   - all records dead: AppendMove expunges them, m.<N> unlinked;
//   - some live: live records copied into a fresh m.<newFileID>,
//     AppendMove rewrites map pointers and expunges dead UIDs under
//     the map X lock, old m.<N> unlinked.
//
// map_uid values are preserved across the move, so per-folder
// indexes keep working with no per-folder I/O.
//
// Safe concurrent with Save/Copy on other folders: the map X lock
// serialises AppendMove against any concurrent AppendBatch.Finish.
func (u *userMailbox) Purge() (PurgeStats, error) {
	stats := PurgeStats{}
	m, err := u.openMap()
	if err != nil {
		return stats, err
	}
	candidates, err := m.GetZeroRefFiles()
	if err != nil {
		return stats, fmt.Errorf("mdbox/purge: enumerate: %w", err)
	}
	// Stable order for reproducible allocation sequences.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })

	for _, fileID := range candidates {
		stats.FilesScanned++
		entries, err := m.RecordsInFile(fileID)
		if err != nil {
			return stats, fmt.Errorf("mdbox/purge: records in m.%d: %w", fileID, err)
		}
		// Partition: live vs dead.
		var live []mdboxmap.MapEntry
		var dead []uint32
		for _, e := range entries {
			if e.RefCount == 0 {
				dead = append(dead, e.UID)
			} else {
				live = append(live, e)
			}
		}

		if len(live) == 0 {
			// Whole file is dead — drop map records and unlink.
			if err := m.AppendMove(nil, dead); err != nil {
				return stats, fmt.Errorf("mdbox/purge: expunge file=%d: %w", fileID, err)
			}
			oldPath := u.mfilePath(fileID)
			fileBytes, _ := u.fileSize(oldPath)
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				return stats, fmt.Errorf("mdbox/purge: unlink m.%d: %w", fileID, err)
			}
			stats.FilesUnlinked++
			stats.RecordsExpunged += len(dead)
			stats.BytesReclaimed += fileBytes
			continue
		}

		// Compact: copy live records into a fresh m.<newID>.
		newID, err := m.AllocFileID()
		if err != nil {
			return stats, fmt.Errorf("mdbox/purge: alloc file id: %w", err)
		}
		moved, err := u.compactRecords(fileID, newID, live)
		if err != nil {
			return stats, err
		}
		// Anchor create-time so mdbox_rotate_interval applies to the
		// compacted file too (best-effort; must not fail the purge).
		if rerr := m.RecordFileCreated(newID, time.Now().Unix()); rerr != nil {
			slog.Warn("mdbox: record compacted file create-time failed", "user", u.username, "file_id", newID, "err", rerr)
		}
		if err := m.AppendMove(moved, dead); err != nil {
			return stats, fmt.Errorf("mdbox/purge: append-move file=%d→%d: %w", fileID, newID, err)
		}
		oldPath := u.mfilePath(fileID)
		oldBytes, _ := u.fileSize(oldPath)
		newBytes, _ := u.fileSize(u.mfilePath(newID))
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			return stats, fmt.Errorf("mdbox/purge: unlink m.%d: %w", fileID, err)
		}
		stats.FilesRewritten++
		stats.RecordsKept += len(live)
		stats.RecordsExpunged += len(dead)
		// Reclaimed = oldSize - newSize, both taken after AppendMove.
		if delta := oldBytes - newBytes; delta > 0 {
			stats.BytesReclaimed += delta
		}
	}
	return stats, nil
}

// compactRecords copies each live record's body from src m.<id> to
// dst m.<id>, producing the MovedRecord slice AppendMove needs. The
// original GUID from the dbox trailer is preserved: a fresh GUID
// would break message identity across purge cycles.
func (u *userMailbox) compactRecords(srcFileID, dstFileID uint32, live []mdboxmap.MapEntry) ([]mdboxmap.MovedRecord, error) {
	srcPath := u.mfilePath(srcFileID)
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("mdbox/purge: open src m.%d: %w", srcFileID, err)
	}
	defer src.Close()

	dstPath := u.mfilePath(dstFileID)
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mdbox/purge: create dst m.%d: %w", dstFileID, err)
	}
	defer dst.Close()

	out := make([]mdboxmap.MovedRecord, 0, len(live))
	for _, e := range live {
		body, guid, origMbox, err := readRecordBodyAndTrailer(src, e.Offset)
		if err != nil {
			return nil, fmt.Errorf("mdbox/purge: read uid=%d: %w", e.UID, err)
		}
		offset, err := appendRecordToFile(dst, body, guid, origMbox)
		if err != nil {
			return nil, fmt.Errorf("mdbox/purge: write uid=%d: %w", e.UID, err)
		}
		out = append(out, mdboxmap.MovedRecord{
			UID:    e.UID,
			FileID: dstFileID,
			Offset: offset,
			Size:   e.Size,
			GUID:   guid,
		})
	}
	if err := dst.Sync(); err != nil {
		return nil, fmt.Errorf("mdbox/purge: fsync m.%d: %w", dstFileID, err)
	}
	return out, nil
}

// compactRecordsToTier is the tier-aware variant of compactRecords:
// src and dst paths are supplied directly, allowing cross-tier
// copies (primary <-> alt). dstFileID becomes the new file_id in
// the returned MovedRecord slice.
func (u *userMailbox) compactRecordsToTier(srcPath, dstPath string, dstFileID uint32, live []mdboxmap.MapEntry) ([]mdboxmap.MovedRecord, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("mdbox/altmove: open src %s: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mdbox/altmove: create dst %s: %w", dstPath, err)
	}
	defer dst.Close()

	out := make([]mdboxmap.MovedRecord, 0, len(live))
	for _, e := range live {
		body, guid, origMbox, err := readRecordBodyAndTrailer(src, e.Offset)
		if err != nil {
			return nil, fmt.Errorf("mdbox/altmove: read uid=%d: %w", e.UID, err)
		}
		offset, err := appendRecordToFile(dst, body, guid, origMbox)
		if err != nil {
			return nil, fmt.Errorf("mdbox/altmove: write uid=%d: %w", e.UID, err)
		}
		out = append(out, mdboxmap.MovedRecord{
			UID:    e.UID,
			FileID: dstFileID,
			Offset: offset,
			Size:   e.Size,
			GUID:   guid,
		})
	}
	if err := dst.Sync(); err != nil {
		return nil, fmt.Errorf("mdbox/altmove: fsync %s: %w", dstPath, err)
	}
	return out, nil
}

// appendRecordToFile writes a dbox v2 record at end-of-file. guid
// must be the original GUID from the source trailer, preserved
// verbatim so message identity survives compaction. The R timestamp
// is refreshed (last-stored time, not internalDate). Returns the
// record's start offset.
func appendRecordToFile(dst *os.File, body []byte, guid [16]byte, origMailbox string) (uint32, error) {
	pos, err := dst.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	// File-header line precedes only the first record; later records
	// start directly at their message header.
	//
	// The size is ours without asking: every caller opens the destination
	// O_CREATE|O_TRUNC, so compaction always writes the header line itself and
	// the file announces what this binary writes. The save path cannot assume
	// that -- it appends to files it did not create -- and does read M (#1525).
	rec := buildDboxMessageRecord(body, guid, origMailbox, messageHeaderSize)
	if pos == 0 {
		rec = append(buildDboxFileHeader(), rec...)
	}
	if _, err := dst.Write(rec); err != nil {
		return 0, err
	}
	return uint32(pos), nil
}

// fileSize returns the on-disk size of path; missing files
// report 0 with no error.
func (u *userMailbox) fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size(), nil
}
