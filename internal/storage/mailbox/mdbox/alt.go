package mdbox

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// AltMoveQuery filters which messages get moved to (or from) alt
// storage during an AltMove call.
type AltMoveQuery struct {
	// Before moves messages whose InternalDate (R field in the dbox
	// trailer) is strictly before this time. Zero means "all messages".
	Before time.Time

	// Reverse moves messages FROM alt storage back to primary.
	// Default (false) moves primary → alt.
	Reverse bool

	// Mailbox restricts the candidate scan to one folder name. Empty
	// string means "all folders" (default behaviour).
	// Currently unused in the storage layer (mdbox is folder-agnostic);
	// reserved for future per-folder policy.
	Mailbox string
}

// AltMoveStats reports what a single AltMove invocation accomplished.
type AltMoveStats struct {
	// Candidates is the number of map_uids that matched the query.
	Candidates int
	// Moved is the number of map_uids physically relocated.
	Moved int
	// FilesCreated is the number of new m.<N> files written in the
	// destination tier.
	FilesCreated int
	// FilesUnlinked is the number of source m.<N> files removed.
	FilesUnlinked int
	// BytesMoved is the approximate byte volume relocated.
	BytesMoved int64
	// MovedFilenames is the set of Filenames (decimal map_uid strings)
	// that were physically relocated. The caller uses this to update the
	// AltTier flag in the fileindex so Fetch can skip primary open()
	// syscalls for cold-tier messages.
	MovedFilenames []string
}

// AltMove moves messages between primary and alt storage tiers. It:
//
//  1. Scans the source-tier storage for m.<N> files.
//  2. Reads each dbox record's R-field (InternalDate) from the
//     trailer to apply the Before filter.
//  3. Collects the eligible map_uids and groups them by source
//     file_id (so whole files can be evaluated together).
//  4. For each source file that has at least one eligible record,
//     rewrites its live records into the destination tier via
//     compactRecordsToAlt (primary→alt) or compactRecordsFromAlt
//     (alt→primary), updating the global map atomically.
//  5. Unlinks the (now empty or fully-moved) source file.
//
// Only map_uids whose refcount equals the number of eligible copies
// found in the source tier are moved — all instances must be marked
// before physical movement occurs.
// Because yarilo's Copy() preserves the same map_uid across all
// folders, a refcount==1 message is always self-consistent and the
// rule simplifies to: eligibility is per-message, not per-folder.
//
// AltMove is idempotent: if the source file is already gone (a
// previous run moved it), the map lookup will not find records
// pointing there and the call is a no-op.
func (u *userMailbox) AltMove(q AltMoveQuery) (AltMoveStats, error) {
	if !u.AltEnabled() {
		return AltMoveStats{}, fmt.Errorf("mdbox/altmove: alt storage not configured")
	}

	m, err := u.openMap()
	if err != nil {
		return AltMoveStats{}, err
	}

	// Determine source and destination tier paths.
	srcTier := u.storagePath()
	dstTier := u.altStoragePath()
	if q.Reverse {
		srcTier, dstTier = dstTier, srcTier
	}

	if err := os.MkdirAll(dstTier, 0o700); err != nil {
		return AltMoveStats{}, fmt.Errorf("mdbox/altmove: mkdir dst: %w", err)
	}

	// Scan source m.<N> files for eligible records.
	candidates, err := u.scanAltCandidates(m, srcTier, q)
	if err != nil {
		return AltMoveStats{}, fmt.Errorf("mdbox/altmove: scan: %w", err)
	}

	stats := AltMoveStats{Candidates: len(candidates)}
	if len(candidates) == 0 {
		return stats, nil
	}

	// Group candidates by source file_id.
	byFile := groupByFileID(candidates)
	fileIDs := make([]uint32, 0, len(byFile))
	for fid := range byFile {
		fileIDs = append(fileIDs, fid)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	for _, srcFileID := range fileIDs {
		eligibleUIDs := byFile[srcFileID]

		// Load all records in this source file from the map.
		allInFile, err := m.RecordsInFile(srcFileID)
		if err != nil {
			return stats, fmt.Errorf("mdbox/altmove: records in m.%d: %w", srcFileID, err)
		}

		// Partition: records we move vs records that stay.
		eligibleSet := make(map[uint32]bool, len(eligibleUIDs))
		for _, uid := range eligibleUIDs {
			eligibleSet[uid] = true
		}

		var toMove []mdboxmap.MapEntry
		var toStay []mdboxmap.MapEntry
		for _, e := range allInFile {
			if eligibleSet[e.UID] {
				toMove = append(toMove, e)
			} else {
				toStay = append(toStay, e)
			}
		}
		if len(toMove) == 0 {
			continue
		}

		srcPath := mfileInTier(srcTier, srcFileID)

		if len(toStay) == 0 {
			// Whole file moves to dst — compact all records into one
			// new dst file and remove the source.
			newFileID, err := m.AllocFileID()
			if err != nil {
				return stats, fmt.Errorf("mdbox/altmove: alloc file id: %w", err)
			}
			moved, err := u.compactRecordsToTier(srcPath, mfileInTier(dstTier, newFileID), newFileID, toMove)
			if err != nil {
				return stats, err
			}
			if err := m.AppendMove(moved, nil); err != nil {
				return stats, fmt.Errorf("mdbox/altmove: append-move m.%d→m.%d: %w", srcFileID, newFileID, err)
			}
			sz, _ := u.fileSize(srcPath)
			if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
				return stats, fmt.Errorf("mdbox/altmove: unlink m.%d: %w", srcFileID, err)
			}
			stats.Moved += len(moved)
			stats.FilesCreated++
			stats.FilesUnlinked++
			stats.BytesMoved += sz
			for _, e := range moved {
				stats.MovedFilenames = append(stats.MovedFilenames, fmt.Sprintf("%d", e.UID))
			}
			continue
		}

		// Partial: only some records move. We need to:
		// 1. Compact the movers into a new dst file.
		// 2. Compact the stayers into a new src file (to drop the moved gaps).
		// 3. Update the map atomically.
		// 4. Remove the old src file.
		newDstFileID, err := m.AllocFileID()
		if err != nil {
			return stats, fmt.Errorf("mdbox/altmove: alloc dst file id: %w", err)
		}
		newSrcFileID, err := m.AllocFileID()
		if err != nil {
			return stats, fmt.Errorf("mdbox/altmove: alloc src file id: %w", err)
		}

		// Write movers to dst tier.
		movedRecords, err := u.compactRecordsToTier(srcPath, mfileInTier(dstTier, newDstFileID), newDstFileID, toMove)
		if err != nil {
			return stats, err
		}
		// Rewrite stayers into a fresh src file.
		stayRecords, err := u.compactRecordsToTier(srcPath, mfileInTier(srcTier, newSrcFileID), newSrcFileID, toStay)
		if err != nil {
			return stats, err
		}

		allMoved := append(movedRecords, stayRecords...)
		if err := m.AppendMove(allMoved, nil); err != nil {
			return stats, fmt.Errorf("mdbox/altmove: append-move partial m.%d: %w", srcFileID, err)
		}
		sz, _ := u.fileSize(srcPath)
		if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
			return stats, fmt.Errorf("mdbox/altmove: unlink m.%d: %w", srcFileID, err)
		}
		stats.Moved += len(movedRecords)
		for _, e := range movedRecords {
			stats.MovedFilenames = append(stats.MovedFilenames, fmt.Sprintf("%d", e.UID))
		}
		stats.FilesCreated += 2
		stats.FilesUnlinked++
		stats.BytesMoved += sz
	}
	return stats, nil
}

// scanAltCandidates walks every m.<N> file in srcTier, reads each
// dbox trailer to obtain InternalDate (R field), and returns the
// map_uids that satisfy the query filter. Only records whose
// map_uid is still alive in the map (refcount > 0) are returned.
//
// The join between physical records and map_uids is done via
// (fileID, offset): m.RecordsInFile gives the map index for a
// given file_id; scanMFileForAlt gives the physical layout.
// Records not found in the map (orphaned) are silently skipped —
// they will be collected by the next Purge run.
func (u *userMailbox) scanAltCandidates(m *mdboxmap.Map, srcTier string, q AltMoveQuery) ([]altCandidate, error) {
	entries, err := os.ReadDir(srcTier)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", srcTier, err)
	}

	var out []altCandidate
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		var fileID uint32
		if _, err := fmt.Sscanf(de.Name(), "m.%d", &fileID); err != nil {
			continue
		}
		path := mfileInTier(srcTier, fileID)

		// Physical scan: offset → internalDate.
		physRecs, err := scanMFileForAlt(path)
		if err != nil {
			return nil, fmt.Errorf("scan m.%d: %w", fileID, err)
		}
		// Build offset → physRecord index.
		byOffset := make(map[uint32]physRecord, len(physRecs))
		for _, pr := range physRecs {
			byOffset[pr.offset] = pr
		}

		// Map index for this file: offset → MapEntry.
		mapRecs, err := m.RecordsInFile(fileID)
		if err != nil {
			return nil, fmt.Errorf("map records m.%d: %w", fileID, err)
		}
		for _, e := range mapRecs {
			if e.RefCount == 0 {
				continue
			}
			pr, ok := byOffset[e.Offset]
			if !ok {
				continue
			}
			if !q.Before.IsZero() && !pr.internalDate.Before(q.Before) {
				continue
			}
			out = append(out, altCandidate{
				mapUID:   e.UID,
				fileID:   fileID,
				offset:   e.Offset,
				size:     e.Size,
				refCount: e.RefCount,
			})
		}
	}
	return out, nil
}

type altCandidate struct {
	mapUID   uint32
	fileID   uint32
	offset   uint32
	size     uint32
	refCount uint16
}

// groupByFileID groups a slice of altCandidates by their source file_id.
func groupByFileID(cands []altCandidate) map[uint32][]uint32 {
	out := make(map[uint32][]uint32)
	for _, c := range cands {
		out[c.fileID] = append(out[c.fileID], c.mapUID)
	}
	return out
}

// mfileInTier returns the path for m.<fileID> in the given tier directory.
func mfileInTier(tierDir string, fileID uint32) string {
	return fmt.Sprintf("%s/m.%d", tierDir, fileID)
}
