package mdbox

import (
	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// HealCorruptFolder expunges the records whose message is gone and clears the
// FSCKD marker in the same locked scope. An incomplete scan ABORTS it, or a
// message purge just compacted would read as vanished. The vanished message's
// map refcount is not decremented here; the leak is reclaimed by the next
// rebuild and purge.
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
