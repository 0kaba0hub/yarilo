package mdbox

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The index carries a message's map_uid in its own record, as the reference's
// does, so a name is read from the folder rather than from a sidecar (#1700).
var (
	uidIndexOnce sync.Once
	uidIndex     mailbox.IndexBackend
)

// storageIndex is the index backend, built once. Handles for one user are
// shared inside it, so this is the same view the session already has.
func storageIndex() mailbox.IndexBackend {
	uidIndexOnce.Do(func() { uidIndex = indexfile.New() })
	return uidIndex
}

// storageKeys is this handle's own view, opened on first use and closed with it.
func (u *userMailbox) keyReader() (mailbox.StorageKeyReader, mailbox.UserIndex, error) {
	u.keyMu.Lock()
	defer u.keyMu.Unlock()
	if u.keyIdx == nil {
		u.keyIdx = storageIndex().OpenUser(u.info)
	}
	reader, ok := u.keyIdx.(mailbox.StorageKeyReader)
	if !ok {
		return nil, nil, fmt.Errorf("mdbox/by-uid: the index does not carry storage keys")
	}
	return reader, u.keyIdx, nil
}

// PathByUID names the file holding the message: its map_uid, in decimal.
func (u *userMailbox) PathByUID(folder string, uid uint32) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("mdbox/by-uid: uid 0 names no message")
	}
	reader, idx, err := u.keyReader()
	if err != nil {
		return "", err
	}
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		return "", fmt.Errorf("mdbox/by-uid: open %q: %w", folder, err)
	}
	mapUID, _, have := reader.StorageKey(f.ID, uid)
	if !have {
		return "", fmt.Errorf("mdbox/by-uid: %q uid %d: the record names no storage: %w",
			folder, uid, mailbox.ErrCorruptStorage)
	}
	return strconv.FormatUint(uint64(mapUID), 10), nil
}

func (u *userMailbox) OpenByUID(folder string, uid uint32, altTier bool) (io.ReadCloser, error) {
	name, err := u.PathByUID(folder, uid)
	if err != nil {
		return nil, err
	}
	return u.Fetch(folder, name, altTier)
}
