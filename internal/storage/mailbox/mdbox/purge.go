package mdbox

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
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

// Purge reclaims disk in <home>/mdbox/storage by walking every
// m.<N> file that contains at least one zero-ref record. For
// each such file:
//
//   - if every record in the file is zero-ref, AppendMove
//     expunges them all and the m.<N> file is unlinked;
//   - otherwise, the live records are copied forward into a
//     fresh m.<newFileID>, AppendMove rewrites the map pointers
//     to the new location (and expunges the dead UIDs) under
//     the map X lock, then the original m.<N> is unlinked.
//
// `map_uid` values are preserved across the move — every
// per-folder index that references them continues to work
// without any per-folder I/O. This is the canonical mdbox
// "shared-by-default" property.
//
// Purge is safe to invoke concurrently with Save / Copy on
// other folders: the map X lock serialises the AppendMove
// against any concurrent AppendBatch.Finish.
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
	// Stable order so multi-file purges produce reproducible
	// allocation sequences (helps debugging / fixture diffs).
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
		// Reclaimed bytes = oldSize - newSize. Both are taken
		// after AppendMove so live records are accounted for.
		if delta := oldBytes - newBytes; delta > 0 {
			stats.BytesReclaimed += delta
		}
	}
	return stats, nil
}

// compactRecords reads each live record's body (and original GUID)
// from src m.<id> and appends them to dst m.<id>, producing the
// MovedRecord slice AppendMove needs. The original GUID from the
// dbox trailer is preserved in the destination record — minting a
// fresh GUID would break message identity across purge cycles and
// diverge from Dovecot's mdbox_purge_save_msg behaviour which
// copies metadata verbatim.
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
		body, guid, err := readRecordBodyAndTrailer(src, e.Offset)
		if err != nil {
			return nil, fmt.Errorf("mdbox/purge: read uid=%d: %w", e.UID, err)
		}
		offset, err := appendRecordToFile(dst, body, guid)
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

// compactRecordsToTier is the tier-aware variant of compactRecords.
// It reads live records from srcPath and writes them into dstPath
// (which may be in a different storage tier), using dstFileID as
// the new file_id in the returned MovedRecord slice. Unlike
// compactRecords, src and dst paths are supplied directly by the
// caller — this allows cross-tier copies (primary → alt and vice
// versa).
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
		body, guid, err := readRecordBodyAndTrailer(src, e.Offset)
		if err != nil {
			return nil, fmt.Errorf("mdbox/altmove: read uid=%d: %w", e.UID, err)
		}
		offset, err := appendRecordToFile(dst, body, guid)
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

// appendRecordToFile writes a canonical dbox v2 record at the
// current end-of-file. guid must be the original GUID from the
// source trailer — preserved verbatim so message identity
// survives compaction (mirrors Dovecot mdbox_purge_save_msg which
// copies metadata verbatim). The R timestamp is refreshed because
// it reflects when the record was last stored, not the mail
// internalDate (same as Dovecot behaviour). Returns the byte
// offset at which the record starts.
func appendRecordToFile(dst *os.File, body []byte, guid [16]byte) (uint32, error) {
	pos, err := dst.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	rec := buildDboxRecord(body, guid)
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
