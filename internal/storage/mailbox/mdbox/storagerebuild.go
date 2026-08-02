package mdbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// allMessages selects every message in a folder for GetMessages.
var allMessages = mailbox.SeqSet{{From: 1, To: 0}}

// withMapLock runs fn while holding the cross-process storage (map) X-lock at
// the driver level, so a storage-wide rebuild is serialised against deliveries
// that take the same lock inside box.Save. Re-entrant: mdboxmap methods called
// under fn see HoldsResource(MdboxMapKey) and skip re-acquiring.
func (u *userMailbox) withMapLock(fn func() error) error {
	if u.b.locker == nil {
		return fn()
	}
	key := locks.MdboxMapKey(u.username)
	if u.b.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 95*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 90*time.Second)
	if err != nil {
		return fmt.Errorf("mdbox/rebuild: acquire map lock: %w", err)
	}
	defer func() { _ = u.b.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// RebuildStorage rebuilds the whole mdbox storage for the user, implementing
// mailbox.StorageWideRebuilder. Under the storage (map) X-lock it reconciles the
// shared map against the physical m.<N> files, resets every folder index to the
// messages that still exist, recomputes each map record's refcount from the
// actual folder references, and drops map records whose message vanished, then
// bumps the persisted rebuild generation counter.
//
// Refcount recompute: after the folder indexes are reconciled, every live map
// record's refcount is set to the number of folders that reference it. A message
// referenced by no folder becomes zero-ref so the next purge reclaims it, and no
// stale refcount>0 lingers. Such a record is NOT re-filed: without an
// orig-mailbox tag the rebuild cannot tell genuinely-lost mail from
// churn/refcount-leak garbage, and blind adoption mass-resurrects deleted mail.
// Unreferenced-but-present records are reported (UnreferencedZeroref) and
// logged, never resurrected.
//
// Lock order: the map X-lock (MdboxMapKey) is outer, the per-folder mailbox lock
// (MailboxKey, taken inside idx.ResetFolder/GetMessages) is inner — the same
// order every delivery takes (box.Save's map lock before idx append's folder
// lock), so there is no inversion. Holding the map lock blocks new deliveries; a
// delivery past box.Save but not yet past its folder append is counted by the
// phase-2 re-read, so its refcount stays >0.
//
// QUIESCENCE REQUIRED. This operator repair tool must run with the user's
// mailboxes quiesced: no concurrent delivery and no concurrent folder operation
// (CREATE/DELETE/RENAME). The rebuild holds only the map lock, not each folder's
// mailbox lock, so:
//   - a delivery whose box.Save committed just before this rebuild took the lock
//     and whose folder append lands after that folder's phase-2 count would have
//     its refcount recomputed to 0 while a folder references it, and a later
//     purge could reclaim live mail; and
//   - with --restore-orphans, a concurrent DELETE of an orphan's orig-mailbox
//     folder can race openOrCreateFolder.
//
// The windows are small (the phase-2 re-read shrinks the delivery one) but not
// eliminated; closing them fully needs the delivery folder-append and the
// restore to serialise on the map lock too.
//
// modseq/QRESYNC: ResetFolder preserves each surviving record's own modseq (no
// modseq storm for QRESYNC clients); it still loses per-UID VANISHED fidelity
// for dropped records, since the reset rewrites the whole set. The dropped UIDs
// are returned per folder in StorageRebuildStats.ExpungedUIDs so the caller
// invalidates their FTS documents instead of leaving ghosts until the next
// rescan.
func (u *userMailbox) RebuildStorage(idx mailbox.UserIndex, restoreOrphans bool) (mailbox.StorageRebuildStats, error) {
	var stats mailbox.StorageRebuildStats

	// Alt-mounted guard: a configured-but-unmounted alt tier would make every
	// alt-resident message look vanished and get mass-expunged. Refuse.
	if u.AltEnabled() {
		if _, err := os.Stat(u.altStoragePath()); err != nil {
			return stats, fmt.Errorf("mdbox/rebuild: alt storage %q unavailable, refusing to rebuild (would expunge alt-resident mail): %w", u.altStoragePath(), err)
		}
	}

	m, err := u.openMap()
	if err != nil {
		return stats, err
	}

	err = u.withMapLock(func() error {
		// Abort on an incomplete scan: a half-corrupt m.<N> or a transient I/O read
		// is indistinguishable from "message gone", so expunging on it would delete
		// live mail. The wrapped error names the bad file; move/repair it and re-run.
		scanned, serr := u.scanStorage()
		if serr != nil {
			return fmt.Errorf("mdbox/rebuild: refusing to rebuild on an incomplete scan; move or repair the named m.<N> and re-run: %w", serr)
		}
		stats.Scanned = len(scanned)

		// present maps map_uid → scan record (carries OrigMailbox for restore).
		present := make(map[string]*mailbox.ScanRecord, len(scanned))
		presentUIDs := make(map[uint32]bool, len(scanned))
		for i := range scanned {
			fn := scanned[i].Filename
			if fn == "" {
				continue
			}
			present[fn] = &scanned[i]
			if uid, perr := parseFilename(fn); perr == nil {
				presentUIDs[uid] = true
			}
		}

		folders, ferr := u.ListFolders()
		if ferr != nil {
			return fmt.Errorf("mdbox/rebuild: list folders: %w", ferr)
		}

		// Phase 1: reset each folder to the records whose map_uid is still on disk.
		for _, fe := range folders {
			f, oerr := idx.OpenFolder(fe.Name, 0)
			if oerr != nil {
				return fmt.Errorf("mdbox/rebuild: open %q: %w", fe.Name, oerr)
			}
			dropped, rerr := u.resetFolderToPresent(idx, f, present)
			if rerr != nil {
				return rerr
			}
			if len(dropped) > 0 {
				if stats.ExpungedUIDs == nil {
					stats.ExpungedUIDs = make(map[string][]uint32)
				}
				stats.ExpungedUIDs[f.Name] = dropped
			}
			stats.FoldersRebuilt++
		}

		// Phase 2: re-read every folder to build the authoritative reference count
		// per map_uid (post-reset, picking up any in-flight delivery whose folder
		// append landed after its folder's phase-1 reset). The refcount is
		// recomputed to this count.
		refCount := make(map[uint32]int, len(scanned))
		reread := func() error {
			for _, fe := range folders {
				f, oerr := idx.OpenFolder(fe.Name, 0)
				if oerr != nil {
					return fmt.Errorf("mdbox/rebuild: reopen %q: %w", fe.Name, oerr)
				}
				msgs, gerr := idx.GetMessages(f.ID, allMessages)
				if gerr != nil {
					return fmt.Errorf("mdbox/rebuild: reread %q: %w", fe.Name, gerr)
				}
				for _, mm := range msgs {
					if mm.Filename == "" {
						continue
					}
					if uid, perr := parseFilename(mm.Filename); perr == nil {
						refCount[uid]++
					}
				}
			}
			return nil
		}
		if err := reread(); err != nil {
			return err
		}

		// Phase 3 (opt-in): restore orphans that carry an orig-mailbox tag back to
		// their home folder. Only tagged and currently-unreferenced records qualify;
		// an untagged churn/leak record is never re-filed. Restored messages go
		// through AllocateAndAppend, the same path a normal delivery takes, so the
		// destination folder's vsize/modseq/quota aggregates are recomputed
		// correctly.
		if restoreOrphans {
			restored, rerr := u.restoreTaggedOrphans(idx, present, refCount)
			if rerr != nil {
				return rerr
			}
			stats.OrphansRestored = restored
		}

		// Drop map records whose message vanished from storage.
		dropped, derr := m.ExpungeVanished(presentUIDs)
		if derr != nil {
			return fmt.Errorf("mdbox/rebuild: expunge vanished map records: %w", derr)
		}
		stats.Expunged = dropped

		// Recompute refcounts from the actual references (including just-restored
		// orphans): unreferenced records go to zero-ref so purge reclaims them.
		zeroed, rerr := m.SetRefcountsFromReferences(refCount)
		if rerr != nil {
			return fmt.Errorf("mdbox/rebuild: recompute refcounts: %w", rerr)
		}
		stats.UnreferencedZeroref = zeroed
		if zeroed > 0 {
			slog.Warn("mdbox/rebuild: unreferenced messages set zero-ref for purge (NOT resurrected)",
				"user", u.username, "unreferenced", zeroed, "restored", stats.OrphansRestored, "scanned", stats.Scanned)
		}
		return nil
	})
	if err != nil {
		return stats, err
	}

	if err := m.BumpRebuildCount(); err != nil {
		return stats, fmt.Errorf("mdbox/rebuild: bump generation: %w", err)
	}
	stats.RebuildCount = m.RebuildCount()
	return stats, nil
}

// restoreTaggedOrphans re-files every present, currently-unreferenced message
// that carries an orig-mailbox tag back into that folder (created if missing)
// via AllocateAndAppend, the same path a normal delivery takes, so
// vsize/modseq/quota aggregates stay correct. It bumps refCount for each
// restored map_uid so the subsequent recompute keeps it referenced. Untagged
// records are left alone (they become zero-ref for purge). Returns the count
// restored.
func (u *userMailbox) restoreTaggedOrphans(idx mailbox.UserIndex, present map[string]*mailbox.ScanRecord, refCount map[uint32]int) (int, error) {
	// Deterministic order so two runs restore identically.
	fns := make([]string, 0, len(present))
	for fn := range present {
		fns = append(fns, fn)
	}
	sort.Strings(fns)

	openFolders := make(map[string]*mailbox.Folder)
	restored := 0
	for _, fn := range fns {
		rec := present[fn]
		if rec.OrigMailbox == "" {
			continue // no home recorded; never guess, leave for purge
		}
		uid, perr := parseFilename(fn)
		if perr != nil || refCount[uid] != 0 {
			continue // only currently-unreferenced records are orphans
		}
		target, oerr := u.openOrCreateFolder(idx, rec.OrigMailbox, openFolders)
		if oerr != nil {
			return restored, oerr
		}
		// A restored orphan comes back with default flags (unseen, no keywords):
		// flags live in the fileindex, an orphan is a message no index references,
		// and the storage trailer carries no flags, so there is no surviving source.
		nm := &mailbox.MessageMeta{
			Filename:     fn,
			Size:         rec.Size,
			VSize:        rec.VSize,
			InternalDate: rec.InternalDate,
			GUID:         rec.GUID,
		}
		if err := idx.AllocateAndAppend(target.ID, nm); err != nil {
			return restored, fmt.Errorf("mdbox/rebuild: restore %s into %q: %w", fn, rec.OrigMailbox, err)
		}
		refCount[uid]++
		restored++
	}
	return restored, nil
}

// openOrCreateFolder returns an index handle for name, creating the mailbox
// (storage dir + index folder) if it does not exist. Handles are cached in the
// supplied map for the duration of the rebuild.
func (u *userMailbox) openOrCreateFolder(idx mailbox.UserIndex, name string, cache map[string]*mailbox.Folder) (*mailbox.Folder, error) {
	if f, ok := cache[name]; ok {
		return f, nil
	}
	exists, err := u.FolderExists(name)
	if err != nil {
		// Surface a transient stat failure (EIO/NFS) rather than mistaking it for
		// "absent" and later failing with a misleading "folder does not exist".
		return nil, fmt.Errorf("mdbox/rebuild: stat restore target %q: %w", name, err)
	}
	if !exists {
		if cerr := u.Create(name); cerr != nil {
			return nil, fmt.Errorf("mdbox/rebuild: create restore target %q: %w", name, cerr)
		}
	}
	f, err := idx.OpenFolder(name, 0)
	if err != nil {
		return nil, fmt.Errorf("mdbox/rebuild: open restore target %q: %w", name, err)
	}
	cache[name] = f
	return f, nil
}

// resetFolderToPresent rewrites folder f's index to the subset of its current
// records whose map_uid is still present in the physical scan, dropping the rest
// (their message vanished). Surviving records keep their UID, flags, modseq and
// other metadata. Returns the dropped UIDs so the caller can invalidate their
// FTS documents.
func (u *userMailbox) resetFolderToPresent(idx mailbox.UserIndex, f *mailbox.Folder, present map[string]*mailbox.ScanRecord) ([]uint32, error) {
	existing, err := idx.GetMessages(f.ID, allMessages)
	if err != nil {
		return nil, fmt.Errorf("mdbox/rebuild: get messages %q: %w", f.Name, err)
	}
	rebuilt := make([]*mailbox.MessageMeta, 0, len(existing))
	for _, mm := range existing {
		if mm.Filename == "" {
			continue
		}
		if _, ok := present[mm.Filename]; !ok {
			continue // map_uid vanished from storage; drop this record
		}
		rebuilt = append(rebuilt, mm)
	}
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].UID < rebuilt[j].UID })
	expunged, err := idx.ResetFolder(f.ID, rebuilt)
	if err != nil {
		return nil, fmt.Errorf("mdbox/rebuild: reset %q: %w", f.Name, err)
	}
	return expunged, nil
}
