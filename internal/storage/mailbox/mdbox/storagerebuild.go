package mdbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// allMessages selects every message in a folder for GetMessages.
var allMessages = mailbox.SeqSet{{From: 1, To: 0}}

// withMapLock runs fn while holding the cross-process storage (map) X-lock at
// the driver level, so a whole storage-wide rebuild is serialised against the
// deliveries that take the same lock inside box.Save. Re-entrant: the mdboxmap
// methods called under fn see HoldsResource(MdboxMapKey) and skip re-acquiring.
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
// mailbox.StorageWideRebuilder. It reconciles the shared map against the
// physical m.<N> files, resets every folder index to the messages that still
// exist, recomputes each map record's refcount from the actual folder references,
// and drops map records whose message vanished — all under the storage (map)
// X-lock, then bumps the persisted rebuild generation counter.
//
// Refcount recompute (Dovecot rebuild_apply_map parity): after the folder
// indexes are reconciled, every live map record's refcount is set to the number
// of folders that reference it. A message referenced by no folder becomes
// zero-ref, so the next purge reclaims it — no stale refcount>0 lingers to trip
// the next rebuild. It is NOT re-filed into a mailbox: without ORIG_MAILBOX
// metadata (a #594 Phase 2b follow-up) the rebuild cannot tell genuinely-lost
// mail from churn/refcount-leak garbage, and blind adoption mass-resurrects
// deleted mail (a real sandbox finding). Unreferenced-but-present records are
// reported (UnreferencedZeroref) and logged, never resurrected.
//
// Lock order: the map X-lock (MdboxMapKey) is the outer lock and the per-folder
// mailbox lock (MailboxKey, taken inside idx.ResetFolder/GetMessages) is the
// inner — the same order every delivery path takes (box.Save's map lock before
// idx append's folder lock), so there is no inversion. Holding the map lock
// blocks new deliveries; a delivery already past box.Save but not yet past its
// folder append is counted by the phase-2 re-read, so its refcount stays >0.
//
// modseq/QRESYNC: ResetFolder stamps a fresh modseq on every surviving record
// (a modseq storm for QRESYNC clients) and loses VANISHED fidelity for dropped
// UIDs — the same accepted parity gap as the sdbox reactive heal. FTS may retain
// ghost documents for expunged map_uids until the next fts rescan.
func (u *userMailbox) RebuildStorage(idx mailbox.UserIndex) (mailbox.StorageRebuildStats, error) {
	var stats mailbox.StorageRebuildStats

	// Alt-mounted guard: a configured-but-unmounted alt tier would make every
	// alt-resident message look vanished and get mass-expunged. Refuse, as
	// Dovecot's dbox_verify_alt_storage does before a rebuild.
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
		// Scan once. Abort on an incomplete scan: a half-corrupt m.<N> or a
		// transient I/O read is indistinguishable from "message gone", so
		// expunging on it would delete live mail. The wrapped error names the bad
		// file; the operator flow is to move/repair that m.<N> and re-run.
		scanned, serr := u.scanStorage()
		if serr != nil {
			return fmt.Errorf("mdbox/rebuild: refusing to rebuild on an incomplete scan; move or repair the named m.<N> and re-run: %w", serr)
		}
		stats.Scanned = len(scanned)

		present := make(map[string]struct{}, len(scanned))
		presentUIDs := make(map[uint32]bool, len(scanned))
		for i := range scanned {
			fn := scanned[i].Filename
			if fn == "" {
				continue
			}
			present[fn] = struct{}{}
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
			if err := u.resetFolderToPresent(idx, f, present); err != nil {
				return err
			}
			stats.FoldersRebuilt++
		}

		// Phase 2: re-read every folder to build the authoritative reference COUNT
		// per map_uid (post-reset, and picking up any in-flight delivery whose
		// folder append landed after its folder's phase-1 reset). This count is
		// what the refcount is recomputed to.
		refCount := make(map[uint32]int, len(scanned))
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

		// Drop map records whose message vanished from storage.
		dropped, derr := m.ExpungeVanished(presentUIDs)
		if derr != nil {
			return fmt.Errorf("mdbox/rebuild: expunge vanished map records: %w", derr)
		}
		stats.Expunged = dropped

		// Recompute refcounts from the actual references: unreferenced records go
		// to zero-ref so purge reclaims them (never resurrected here).
		zeroed, rerr := m.SetRefcountsFromReferences(refCount)
		if rerr != nil {
			return fmt.Errorf("mdbox/rebuild: recompute refcounts: %w", rerr)
		}
		stats.UnreferencedZeroref = zeroed
		if zeroed > 0 {
			slog.Warn("mdbox/rebuild: unreferenced messages set zero-ref for purge (NOT resurrected — orphan restore lands with ORIG_MAILBOX)",
				"user", u.username, "unreferenced", zeroed, "scanned", stats.Scanned)
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

// resetFolderToPresent rewrites folder f's index to the subset of its current
// records whose map_uid is still present in the physical scan, dropping the rest
// (their message vanished). Surviving records keep their UID, flags and other
// metadata; ResetFolder re-stamps modseq.
func (u *userMailbox) resetFolderToPresent(idx mailbox.UserIndex, f *mailbox.Folder, present map[string]struct{}) error {
	existing, err := idx.GetMessages(f.ID, allMessages)
	if err != nil {
		return fmt.Errorf("mdbox/rebuild: get messages %q: %w", f.Name, err)
	}
	rebuilt := make([]*mailbox.MessageMeta, 0, len(existing))
	for _, mm := range existing {
		if mm.Filename == "" {
			continue
		}
		if _, ok := present[mm.Filename]; !ok {
			continue // map_uid vanished from storage — drop this record
		}
		rebuilt = append(rebuilt, mm)
	}
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].UID < rebuilt[j].UID })
	if err := idx.ResetFolder(f.ID, rebuilt); err != nil {
		return fmt.Errorf("mdbox/rebuild: reset %q: %w", f.Name, err)
	}
	return nil
}
