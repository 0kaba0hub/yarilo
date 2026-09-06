package mdbox

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
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

	// Mailbox restricts the scan to one folder; empty means all. Unused by
	// this folder-agnostic driver, reserved for per-folder policy.
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
	// MovedFilenames names what was relocated, so the caller can flag AltTier in
	// the index and Fetch can skip the primary open.
	MovedFilenames []string
}

// AltMove rewrites each source file's eligible records into the other tier under
// the map lock and unlinks the source. Eligibility is per message, since Copy
// keeps one map_uid across folders. Idempotent.
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

		// Partial: movers into a new dst file, stayers into a new src file, the
		// map updated atomically, the old src removed.
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

// scanAltCandidates returns the live map_uids in srcTier whose trailer date
// satisfies the filter, joining physical records to the map by (fileID, offset).
// An orphan the map does not know is skipped for the next purge.
func (u *userMailbox) scanAltCandidates(m *mdboxmap.Map, srcTier string, q AltMoveQuery) ([]altCandidate, error) {
	countDirReads.Add(1)
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
