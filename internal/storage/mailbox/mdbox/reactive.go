package mdbox

import (
	"errors"
	"fmt"
	"sync"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ErrHealDeferred says the heal was not attempted: it already failed at this
// storage generation, and nothing has rebuilt since.
var ErrHealDeferred = errors.New("mdbox/heal: already failed at this rebuild count")

// healBarrier holds, per process and per folder, the generation at which a heal
// failed on unreadable bytes. Ours, not the reference's (#1682).
var healBarrier sync.Map // string -> uint32

func healBarrierKey(user string, folderID uint64) string {
	return fmt.Sprintf("%s\x00%d", user, folderID)
}

// beforeHealScan runs inside both locks, before the scan. Test seam: what the
// serialisation row counts.
var beforeHealScan func()

// HealCorruptFolder expunges the records whose message is gone and clears the
// FSCKD marker in the same locked scope. An incomplete scan ABORTS it, or a
// message purge just compacted would read as vanished. The vanished message's
// map refcount is not decremented here; the leak is reclaimed by the next
// rebuild and purge.
//
// Lock order: map outer, folder inner, as delivery takes them -- Scan walks the
// whole storage, not one folder (#1682).
func (u *userMailbox) HealCorruptFolder(idx mailbox.UserIndex, folder *mailbox.Folder) ([]uint32, error) {
	var expunged []uint32
	err := u.withMapLock(func() error {
		gen, gerr := u.storageGeneration()
		if gerr != nil {
			return gerr
		}
		key := healBarrierKey(u.username, folder.ID)
		if v, ok := healBarrier.Load(key); ok {
			if last, ok := v.(uint32); ok && last == gen {
				return ErrHealDeferred
			}
		}
		return u.withMailboxLock(folder.Name, func() error {
			if beforeHealScan != nil {
				beforeHealScan()
			}
			var e error
			expunged, e = idxrebuild.ExpungeMissing(u, idx, folder)
			if e != nil {
				// Only unreadable bytes bar a retry: a scan a purge kept from
				// finishing says nothing about the folder.
				if errors.Is(e, errScanCorrupt) {
					healBarrier.Store(key, gen)
				}
				return e
			}
			if cm, ok := idx.(mailbox.CorruptionMarker); ok {
				return cm.ClearFolderCorrupt(folder.ID)
			}
			return nil
		})
	})
	return expunged, err
}

// storageGeneration is the map's rebuild counter, re-read under the map lock
// this call already holds.
func (u *userMailbox) storageGeneration() (uint32, error) {
	m, err := u.openMap()
	if err != nil {
		return 0, fmt.Errorf("mdbox/heal: open map: %w", err)
	}
	return m.RebuildCountUnderCallersLock()
}
