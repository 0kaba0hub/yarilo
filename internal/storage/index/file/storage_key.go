package file

import (
	"sort"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// StorageKeys answers a driver asking where messages are. Records are sorted by
// uid, so each answer is a search and not a walk (#1700).
func (u *userIndex) StorageKeys(folderID uint64, uids []uint32) (map[uint32]uint32, error) {
	out := make(map[uint32]uint32, len(uids))
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		recs := fs.file.Records
		for _, uid := range uids {
			i := sort.Search(len(recs), func(i int) bool { return recs[i].UID >= uid })
			if i == len(recs) || recs[i].UID != uid {
				continue
			}
			if mapUID, _ := decodeMdboxRec(recs[i].Ext[extNameMdbox]); mapUID != 0 {
				out[uid] = mapUID
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StorageKey is the single-message form.
func (u *userIndex) StorageKey(folderID uint64, uid uint32) (mapUID, saveDate uint32, ok bool) {
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		recs := fs.file.Records
		i := sort.Search(len(recs), func(i int) bool { return recs[i].UID >= uid })
		if i < len(recs) && recs[i].UID == uid {
			mapUID, saveDate = decodeMdboxRec(recs[i].Ext[extNameMdbox])
			ok = mapUID != 0
		}
		return nil
	})
	if err != nil {
		return 0, 0, false
	}
	return mapUID, saveDate, ok
}

var _ mailbox.StorageKeyReader = (*userIndex)(nil)
