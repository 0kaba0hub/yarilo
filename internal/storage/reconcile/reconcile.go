// Package reconcile imports messages that appeared in a mailbox out of band
// (MDA delivery into new/, another MUA moving a file into cur/) and drops
// index records whose backing file has vanished. It is the routine
// sync-on-SELECT path for drivers whose on-disk representation can change
// without going through yarilo (maildir).
//
// Unlike the admin rebuild flow, this path treats the index as authoritative
// for everything about a message that is already tracked: an existing record
// whose file is still present is left completely untouched (UID, flags,
// keywords, modseq). The physical scan only answers two questions — which
// files are new, and which tracked files are gone. Flags of tracked messages
// are never re-derived from the filename, because in yarilo's maildir model
// flags live in the index and the filename trailer is frozen at delivery time;
// re-deriving them on every SELECT would revert every flag change a client
// made.
package reconcile

import (
	"fmt"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Stats reports what a SyncNew reconcile changed.
type Stats struct {
	Scanned  int
	Imported int
	Expunged int
	// Changed is false when the scan matched the index exactly and no index
	// write was performed (the cheap common path).
	Changed bool
}

// SyncNew reconciles idx's record set for folder against the physical files
// reported by box.Scan: files with no index record are imported with a fresh
// UID (and the delivery-time flags carried in the filename), and index records
// whose file has vanished are expunged. Every already-tracked record whose
// file is still present is preserved verbatim.
//
// The caller must already hold the per-folder write lock and pass the opened
// folder handle. When nothing was added or removed no index write happens and
// Stats.Changed is false.
func SyncNew(box mailbox.UserMailbox, idx mailbox.UserIndex, folder *mailbox.Folder) (Stats, error) {
	var stats Stats

	scanned, err := box.Scan(folder.Name)
	if err != nil {
		return stats, fmt.Errorf("reconcile/scan: %w", err)
	}
	stats.Scanned = len(scanned)

	onDisk := make(map[string]*mailbox.ScanRecord, len(scanned))
	for i := range scanned {
		if scanned[i].Filename != "" {
			onDisk[scanned[i].Filename] = &scanned[i]
		}
	}

	existing, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return stats, fmt.Errorf("reconcile/get messages: %w", err)
	}

	// Preserve every tracked record whose file is still on disk, in index
	// order. Drop the rest (file vanished out of band).
	merged := make([]*mailbox.MessageMeta, 0, len(existing)+len(scanned))
	tracked := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		if m.Filename == "" {
			// A record with no filename cannot be matched against the scan;
			// keep it rather than silently dropping data.
			merged = append(merged, m)
			continue
		}
		if _, ok := onDisk[m.Filename]; ok {
			tracked[m.Filename] = struct{}{}
			merged = append(merged, m)
		} else {
			stats.Expunged++
		}
	}

	nextUID := folder.NextUID
	if nextUID == 0 {
		nextUID = 1
	}
	// Import files the index does not know yet, in scan order so UID
	// assignment is deterministic for a given directory listing.
	for i := range scanned {
		rec := &scanned[i]
		if rec.Filename == "" {
			continue
		}
		if _, ok := tracked[rec.Filename]; ok {
			continue
		}
		merged = append(merged, &mailbox.MessageMeta{
			UID:          nextUID,
			Filename:     rec.Filename,
			Size:         rec.Size,
			VSize:        rec.VSize,
			InternalDate: rec.InternalDate,
			GUID:         rec.GUID,
			Flags:        rec.Flags,
		})
		nextUID++
		stats.Imported++
	}

	if stats.Imported == 0 && stats.Expunged == 0 {
		return stats, nil
	}

	if err := idx.ResetFolder(folder.ID, merged); err != nil {
		return stats, fmt.Errorf("reconcile/reset folder: %w", err)
	}
	stats.Changed = true
	return stats, nil
}
