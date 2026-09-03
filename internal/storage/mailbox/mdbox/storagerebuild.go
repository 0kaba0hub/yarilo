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

// withMapLock holds the map X lock across fn, serialising a rebuild against
// deliveries. Re-entrant: map methods under fn see HoldsResource and skip.
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

// RebuildStorage reconciles the map against the m.<N> files and recomputes every
// refcount from folder references. An unreferenced record goes to zero and is
// never re-filed: untagged, lost mail and leak garbage look alike. QUIESCENCE
// REQUIRED -- only the map lock is held.
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

		// Phase 3, opt-in: tagged orphans go home; an untagged one is never
		// re-filed.
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

// restoreTaggedOrphans re-files every unreferenced message carrying an
// orig-mailbox tag, through the same path a delivery takes so the folder's
// aggregates stay right. Untagged records are left for purge.
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

// resetFolderToPresent drops the folder's records whose map_uid the scan did not
// find, keeping everything else intact, and returns the dropped UIDs.
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
