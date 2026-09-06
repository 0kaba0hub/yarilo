package file

import (
	"encoding/binary"
	"os"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A folder whose message files are named by uid says so in its own index rather
// than in a file beside the mail: the mail directory holds messages (#1704).
const (
	extNameUIDNames = "uid-names"
	uidNamesSize    = 4
	// uidNamesPass is the pass a folder must have been through. Raised when the
	// pass learns more: version 1 orphaned the body of a nameless record (#1713).
	uidNamesPass = 2
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
		if err := fs.markUIDNamedLocked(); err != nil {
			return err
		}
		if err := fs.flush(true); err != nil {
			return err
		}
		return fs.dropSidecarLocked()
	})
}

// markUIDNamedLocked stamps the marker in memory; the caller flushes.
func (fs *folderState) markUIDNamedLocked() error {
	{
		if uidNamedLocked(fs) {
			return nil
		}
		data := make([]byte, uidNamesSize)
		binary.LittleEndian.PutUint32(data, uidNamesPass)
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
		return nil
	}
}

// sidecarExists is the cheap half of the question, asked before any lock.
func sidecarExists(indexDir string) bool {
	_, err := os.Stat(namesPath(indexDir))
	return err == nil
}

// dropSidecarLocked removes it: a folder whose records name their own storage
// has no second place for a name to live (#1700).
func (fs *folderState) dropSidecarLocked() error {
	if !uidNamedLocked(fs) {
		return nil
	}
	fs.filenames = map[uint32]string{}
	if fs.namesFD != nil {
		_ = fs.namesFD.Close()
		fs.namesFD = nil
	}
	if err := os.Remove(namesPath(fs.indexDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func uidNamedLocked(fs *folderState) bool {
	ext := findExt(fs.file.Extensions, extNameUIDNames)
	if ext == nil || len(ext.HdrData) < uidNamesSize {
		return false
	}
	return binary.LittleEndian.Uint32(ext.HdrData) >= uidNamesPass
}

var _ mailbox.UIDNameMarker = (*userIndex)(nil)

// MarkUIDNamedPass stamps an arbitrary pass number. Test seam: a folder marked
// by the older pass is the state #1713 has to reach.
func (u *userIndex) MarkUIDNamedPass(folderID uint64, pass uint32) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		data := make([]byte, uidNamesSize)
		binary.LittleEndian.PutUint32(data, pass)
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
