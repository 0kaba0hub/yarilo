package mdbox

import (
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames moves what an older build kept in the index's sidecar into
// the records: an mdbox name is the map_uid, so the record can hold it and the
// file beside the index has nothing left to say (#1700).
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	adopter, ok := idx.(mailbox.StoredNameAdopter)
	if !ok {
		return 0, nil
	}
	err := adopter.AdoptStoredNames(folder.ID, func(name string) (uint32, bool) {
		id, perr := strconv.ParseUint(name, 10, 32)
		if perr != nil || id == 0 {
			return 0, false
		}
		return uint32(id), true
	})
	return 0, err
}
