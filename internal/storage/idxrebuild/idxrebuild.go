// Package idxrebuild holds the driver-agnostic folder index rebuild core:
// scan the physical storage (UserMailbox.Scan), match the result against the
// current index by filename to preserve UIDs, and atomically replace the record
// set (UserIndex.ResetFolder). It is the single implementation shared by the
// operator rebuild endpoint (backend-api) and the reactive auto-rebuild that
// fires when a dbox driver detects a missing/corrupt message.
//
// The caller is responsible for the cross-process mailbox lock and for opening
// the folder handle; RebuildFolder only reads and rewrites the record set.
package idxrebuild

import (
	"fmt"
	"sort"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Stats reports what a RebuildFolder pass did.
type Stats struct {
	Scanned        int
	UIDsPreserved  int
	UIDsAssigned   int
	OrphansDropped int
}

// RebuildFolder regenerates idx's record set for folder from the physical
// storage reported by box.Scan. Files the index already knows keep their UID
// (and, for dbox, their prior flags/keywords since the driver returns none);
// files the index has never seen are assigned a fresh UID from folder.NextUID
// upward; index records whose file has vanished are dropped. ResetFolder bumps
// NextUID past the highest surviving UID.
//
// The scan error is returned verbatim so the caller can classify it (e.g. a
// driver that has not implemented Scan yet).
func RebuildFolder(box mailbox.UserMailbox, idx mailbox.UserIndex, folder *mailbox.Folder) (Stats, error) {
	var stats Stats

	scanned, err := box.Scan(folder.Name)
	if err != nil {
		return stats, fmt.Errorf("idxrebuild/scan: %w", err)
	}
	stats.Scanned = len(scanned)

	existing, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return stats, fmt.Errorf("idxrebuild/get messages: %w", err)
	}

	byFilename := make(map[string]*mailbox.MessageMeta, len(existing))
	for _, m := range existing {
		if m.Filename != "" {
			byFilename[m.Filename] = m
		}
	}

	nextUID := folder.NextUID
	if nextUID == 0 {
		nextUID = 1
	}
	rebuilt := make([]*mailbox.MessageMeta, 0, len(scanned))
	for i := range scanned {
		rec := &scanned[i]
		if rec.Filename == "" {
			stats.OrphansDropped++
			continue
		}
		newMeta := &mailbox.MessageMeta{
			Filename:     rec.Filename,
			Size:         rec.Size,
			VSize:        rec.VSize,
			InternalDate: rec.InternalDate,
			GUID:         rec.GUID,
		}
		if old, ok := byFilename[rec.Filename]; ok {
			newMeta.UID = old.UID
			// Driver-provided flags (maildir) win since the filename trailer is
			// the source of truth there; dbox returns empty so the index keeps
			// its prior flag set unchanged.
			if len(rec.Flags) > 0 {
				newMeta.Flags = rec.Flags
				newMeta.Keywords = nil
			} else {
				newMeta.Flags = old.Flags
				newMeta.Keywords = old.Keywords
			}
			// Preserve the AltTier marker so Fetch of alt-moved mail still
			// skips the primary open after a rebuild.
			newMeta.AltTier = old.AltTier
			// Preserve GUID when the driver did not stamp one (maildir).
			var zero [16]byte
			if newMeta.GUID == zero {
				newMeta.GUID = old.GUID
			}
			stats.UIDsPreserved++
		} else {
			newMeta.UID = nextUID
			nextUID++
			newMeta.Flags = rec.Flags
			stats.UIDsAssigned++
		}
		rebuilt = append(rebuilt, newMeta)
	}

	// Deterministic on-disk order so two consecutive rebuilds with the same
	// input produce byte-identical .index files.
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].UID < rebuilt[j].UID })

	if err := idx.ResetFolder(folder.ID, rebuilt); err != nil {
		return stats, fmt.Errorf("idxrebuild/reset folder: %w", err)
	}
	return stats, nil
}
