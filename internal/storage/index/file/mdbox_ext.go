package file

import (
	"encoding/binary"
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// The mdbox extension: a message's storage key, kept in the mailbox index the
// way the reference keeps it -- map_uid then save_date, four bytes each.
const (
	extNameMdbox = "mdbox"
	mdboxRecSize = 8
)

func encodeMdboxRec(mapUID, saveDate uint32) []byte {
	b := make([]byte, mdboxRecSize)
	binary.LittleEndian.PutUint32(b[0:4], mapUID)
	binary.LittleEndian.PutUint32(b[4:8], saveDate)
	return b
}

// decodeMdboxRec reads it. A short or absent field yields zeros, which read as
// "this record does not say", the answer every index written before it gives.
func decodeMdboxRec(b []byte) (mapUID, saveDate uint32) {
	if len(b) < mdboxRecSize {
		return 0, 0
	}
	return binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint32(b[4:8])
}

// ensureMdboxExtLocked declares the extension on an index written before it, so
// the field the records carry has a layout. Caller holds fs.mu.
func (fs *folderState) ensureMdboxExtLocked() {
	if findExt(fs.file.Extensions, extNameMdbox) != nil {
		return
	}
	fs.file.Extensions = append(fs.file.Extensions, mailindex.Extension{
		Name: extNameMdbox, RecordSize: mdboxRecSize, RecordAlign: 4,
		ResetID: fs.file.Header.UIDValidity,
	})
	if err := fs.syncHeaderSizeLocked(); err != nil {
		slog.Warn("fileindex: mdbox extension not declared", "folder", fs.folder, "err", err)
	}
}
