package mdbox

import (
	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// HealCorruptFolder is the reactive self-heal for mdbox (implements
// mailbox.ReactiveHealer): a folder flagged FSCKD is repaired on the
// next open. Under the folder's cross-process mailbox lock it expunges
// only the index records whose message is gone from storage (targeted
// ExpungeMissing: QRESYNC tombstone, no ResetFolder, no UID reassign,
// so it cannot race a concurrent delivery), then clears the marker in
// the SAME lock scope. Returns the expunged records.
//
// Per-folder and targeted, NOT the storage-wide rebuild: no refcount
// recompute, no other folders touched, so the rebuild's quiescence
// requirement does not apply.
//
// Structural mirror of sdbox HealCorruptFolder (same lock +
// ExpungeMissing + ClearFolderCorrupt order). If a third dbox driver
// appears, extract a shared helper instead of a third copy.
//
// Concurrency vs purge/altmove: both write the new m.<N> before
// unlinking the old one. If the scan snapshot (os.ReadDir) lists the
// old file but opens it after the unlink, the scan returns
// mailbox.ErrScanIncomplete and ExpungeMissing ABORTS, so a message
// compacted to a new m.<N> is never mistaken for vanished. The heal
// retries on the next open. (Corollary: a near-continuous purge can
// keep the heal in the race window; if a folder stays FSCKD, check
// whether a purge is running.)
//
// Caveats shared with the rebuild: a structurally corrupt m.<N>
// record also makes the scan incomplete, so the heal aborts until the
// bad file is moved aside; and the vanished message's map refcount is
// not decremented here (a leak the next rebuild + purge reclaims).
func (u *userMailbox) HealCorruptFolder(idx mailbox.UserIndex, folder *mailbox.Folder) ([]uint32, error) {
	var expunged []uint32
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
