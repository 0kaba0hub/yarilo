package file

import (
	"encoding/binary"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A folder whose message files are named by uid says so in its own index rather
// than in a file beside the mail: the mail directory holds messages (#1704).
const (
	extNameUIDNames = "uid-names"
	uidNamesSize    = 4
	uidNamesDone    = 1
)

// UIDNamed answers from the header a folder open already read.
func (u *userIndex) UIDNamed(folderID uint64) (bool, error) {
	var done bool
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		done = uidNamedLocked(fs)
		return nil
	})
	return done, err
}

// MarkUIDNamed records that the pass has run, so it runs once.
func (u *userIndex) MarkUIDNamed(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if uidNamedLocked(fs) {
			return nil
		}
		data := make([]byte, uidNamesSize)
		binary.LittleEndian.PutUint32(data, uidNamesDone)
		if ext := findExt(fs.file.Extensions, extNameUIDNames); ext != nil {
			ext.HdrData, ext.HdrSize = data, uidNamesSize
		} else {
			fs.file.Extensions = append(fs.file.Extensions, mailindex.Extension{
				Name: extNameUIDNames, HdrSize: uidNamesSize, HdrData: data,
				RecordAlign: 4, ResetID: fs.file.Header.UIDValidity,
			})
			if err := fs.syncHeaderSizeLocked(); err != nil {
				return err
			}
		}
		return fs.flush(true)
	})
}

func uidNamedLocked(fs *folderState) bool {
	ext := findExt(fs.file.Extensions, extNameUIDNames)
	if ext == nil || len(ext.HdrData) < uidNamesSize {
		return false
	}
	return binary.LittleEndian.Uint32(ext.HdrData) == uidNamesDone
}

var _ mailbox.UIDNameMarker = (*userIndex)(nil)
