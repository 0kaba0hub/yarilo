package quota

import (
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// FolderVSizer is the slice of the index a count-based quota read needs:
// resolve a folder name to a handle and read its aggregate virtual size.
// Satisfied by mailbox.UserIndex.
type FolderVSizer interface {
	OpenFolder(folder string, uidValidity uint32) (*mailbox.Folder, error)
	FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error)
}

// CountUsage sums the index-derived aggregate virtual size and message count
// across the given folders — the authoritative quota usage (the count backend).
// Folders configured as "ignore" by limits are skipped so their messages do not
// count toward quota. Unreadable or absent folders are skipped rather than
// failing the whole read, mirroring how the aggregate self-heals: a transient
// per-folder error must not deny service on the user-wide total.
func CountUsage(idx FolderVSizer, folders []string, limits Limits) Usage {
	var u Usage
	for _, name := range folders {
		if _, ignore := limits.EffectiveLimits(name); ignore {
			continue
		}
		f, err := idx.OpenFolder(name, 0)
		if err != nil {
			continue
		}
		bytes, msgs, err := idx.FolderVSize(f.ID)
		if err != nil {
			continue
		}
		u.StorageBytes += int64(bytes)
		u.Messages += int64(msgs)
	}
	return u
}
