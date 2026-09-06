package file

import (
	"strconv"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// derivesName reports whether a driver finds a message from its uid alone: an
// sdbox message is u.<uid>, an mdbox one is named by the map_uid in its record.
func derivesName(driver string) bool {
	switch driver {
	case "sdbox", "dbox", "mdbox":
		return true
	}
	return false
}

// nameOf answers what a message is called: derived from the record where the
// driver allows it, and read from the sidecar where it does not (#1700).
func (fs *folderState) nameOf(rec *mailindex.Record) string {
	if !fs.derivedName {
		return fs.filenames[rec.UID]
	}
	if mapUID, _ := decodeMdboxRec(rec.Ext[extNameMdbox]); mapUID != 0 {
		return strconv.FormatUint(uint64(mapUID), 10)
	}
	if fs.driverIsSdbox {
		return "u." + strconv.FormatUint(uint64(rec.UID), 10)
	}
	return fs.filenames[rec.UID]
}
