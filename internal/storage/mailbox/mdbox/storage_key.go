package mdbox

import "time"

// StorageKey reads the map_uid out of the name Save returned, so the record can
// carry it the way the reference's does: the name is then derived from the
// index rather than kept beside it (#1700).
func (u *userMailbox) StorageKey(_, filename string) (mapUID, saveDate uint32, ok bool) {
	id, err := parseFilename(filename)
	if err != nil {
		return 0, 0, false
	}
	return id, uint32(time.Now().Unix()), true
}
