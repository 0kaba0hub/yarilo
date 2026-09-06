package mdbox

import (
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames moves what an older build kept in the sidecar into the
// records: an mdbox name is the map_uid, which the record can hold (#1700).
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	adopter, ok := idx.(mailbox.StoredNameAdopter)
	if !ok {
		return 0, nil
	}
	m, err := u.openMap()
	if err != nil {
		return 0, err
	}
	err = adopter.AdoptStoredNames(folder.ID, func(name string, guid [16]byte) (uint32, bool) {
		if id, perr := strconv.ParseUint(name, 10, 32); perr == nil && id != 0 {
			return uint32(id), true
		}
		// No name to read: the map knows the message by the guid the record
		// carries, and this pass is the one chance to ask (#1713).
		entry, ok, lerr := m.LookupByGUID(guid)
		if lerr != nil || !ok {
			return 0, false
		}
		return entry.UID, true
	})
	return 0, err
}
