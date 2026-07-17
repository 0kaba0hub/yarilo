package mdbox

import (
	"context"
	"fmt"
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
// exist, adopts orphans (messages referenced by no folder) into INBOX, and drops
// map records whose message vanished — all under the storage (map) X-lock, then
// bumps the persisted rebuild generation counter.
//
// Lock order: the map X-lock (MdboxMapKey) is the outer lock and the per-folder
// mailbox lock (MailboxKey, taken inside idx.ResetFolder/GetMessages) is the
// inner — the same order every delivery path takes (box.Save's map lock before
// idx append's folder lock), so there is no inversion.
//
// Concurrency caveat: holding the map lock blocks new deliveries (their box.Save
// takes it first), so no new map records appear mid-rebuild. A delivery already
// past box.Save but not yet past its folder append is caught by the phase-2
// re-read below; a delivery whose folder append lands in the tiny window between
// that re-read and orphan adoption could be adopted into INBOX as a duplicate —
// run the rebuild when delivery is quiesced. A duplicate is recoverable (a later
// rebuild reconciles it); no mail is lost.
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

		referenced := make(map[string]bool, len(scanned))

		// Phase 1: reset each folder to the records whose map_uid is still on disk.
		for _, fe := range folders {
			f, oerr := idx.OpenFolder(fe.Name, 0)
			if oerr != nil {
				return fmt.Errorf("mdbox/rebuild: open %q: %w", fe.Name, oerr)
			}
			if err := u.resetFolderToPresent(idx, f, present, referenced); err != nil {
				return err
			}
			stats.FoldersRebuilt++
		}

		// Phase 2: re-read every folder to pick up an in-flight delivery whose
		// folder append landed after its folder's phase-1 reset (its box.Save
		// committed the map record before we took the lock). This keeps such a
		// message from being mistaken for an orphan and duplicated into INBOX.
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
				if mm.Filename != "" {
					referenced[mm.Filename] = true
				}
			}
		}

		// Adopt orphans (present on disk, referenced by no folder) into INBOX.
		orphans := make([]string, 0)
		for fn := range present {
			if !referenced[fn] {
				orphans = append(orphans, fn)
			}
		}
		if len(orphans) > 0 {
			sort.Strings(orphans)
			inbox, oerr := idx.OpenFolder("INBOX", 0)
			if oerr != nil {
				return fmt.Errorf("mdbox/rebuild: open INBOX for orphan adoption: %w", oerr)
			}
			for _, fn := range orphans {
				rec := present[fn]
				nm := &mailbox.MessageMeta{
					Filename:     fn,
					Size:         rec.Size,
					VSize:        rec.VSize,
					InternalDate: rec.InternalDate,
					GUID:         rec.GUID,
				}
				if err := idx.AllocateAndAppend(inbox.ID, nm); err != nil {
					return fmt.Errorf("mdbox/rebuild: adopt orphan %s into INBOX: %w", fn, err)
				}
				stats.OrphansAdopted++
			}
		}

		// Drop map records whose message vanished from storage.
		dropped, derr := m.ExpungeVanished(presentUIDs)
		if derr != nil {
			return fmt.Errorf("mdbox/rebuild: expunge vanished map records: %w", derr)
		}
		stats.Expunged = dropped
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
// metadata; ResetFolder re-stamps modseq. Every survivor's map_uid is added to
// referenced so orphan detection can tell what is still owned by a folder.
func (u *userMailbox) resetFolderToPresent(idx mailbox.UserIndex, f *mailbox.Folder, present map[string]*mailbox.ScanRecord, referenced map[string]bool) error {
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
		referenced[mm.Filename] = true
	}
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].UID < rebuilt[j].UID })
	if err := idx.ResetFolder(f.ID, rebuilt); err != nil {
		return fmt.Errorf("mdbox/rebuild: reset %q: %w", f.Name, err)
	}
	return nil
}
