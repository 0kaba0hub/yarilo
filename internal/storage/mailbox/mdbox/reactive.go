package mdbox

import (
	"github.com/0kaba0hub/yarilo/internal/storage/idxrebuild"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// HealCorruptFolder is the reactive self-heal for mdbox, implementing
// mailbox.ReactiveHealer so a folder flagged FSCKD (a prior read hit a
// missing/corrupt message) is repaired on the next open — the same machinery
// that heals sdbox. Under the folder's cross-process mailbox lock it expunges
// only the folder's index records whose message is no longer present in storage
// (targeted ExpungeMessage — QRESYNC tombstone, no ResetFolder and no UID
// reassignment, so it cannot race a concurrent delivery), then clears the marker
// in the SAME lock scope. Returns the number of records expunged.
//
// This is deliberately per-folder and targeted, NOT the storage-wide rebuild:
// it makes the folder readable again without recomputing refcounts or touching
// other folders, so the quiescence the storage rebuild needs does not apply here.
//
// Concurrency vs purge/altmove: both rewrite an m.<N> by writing the new file
// first and only then unlinking the old one. If this heal's scan snapshot
// (os.ReadDir) still lists the old file but opens it after the unlink, the scan
// returns mailbox.ErrScanIncomplete and ExpungeMissing ABORTS — so a message
// concurrently compacted to a new m.<N> is never mistaken for "vanished" and
// expunged. The heal simply retries on the next open. (Corollary: a near-
// continuous purge/altmove could keep this heal in the race window so it never
// reports success — if a folder stays FSCKD, check whether a purge is running.)
//
// Caveats shared with the storage rebuild: a structurally corrupt m.<N> record
// makes the scan incomplete too, so the heal aborts until the operator moves the
// bad file aside; and the vanished message's map refcount is not decremented
// here (a leak the next operator rebuild + purge reclaims) — the heal's job is
// to make the folder readable, not perfect refcount hygiene.
func (u *userMailbox) HealCorruptFolder(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	var expunged int
	err := u.withMailboxLock(folder.Name, func() error {
		var e error
		expunged, e = idxrebuild.ExpungeMissing(u, idx, folder)
		if e != nil {
			return e
		}
		if cm, ok := idx.(mailbox.CorruptionMarker); ok {
			return cm.ClearFolderCorrupt(folder.ID)
		}
		return nil
	})
	return expunged, err
}
