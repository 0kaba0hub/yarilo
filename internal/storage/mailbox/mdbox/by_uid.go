package mdbox

import (
	"fmt"
	"io"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An mdbox message is named by the map_uid its own index record carries, as the
// reference's does: the record is read once by whoever loaded it (#1700).
func (u *userMailbox) RecordPath(folder string, m *mailbox.MessageMeta) (string, error) {
	if m.MapUID == 0 {
		return "", fmt.Errorf("mdbox/by-uid: %q uid %d: the record names no storage: %w",
			folder, m.UID, mailbox.ErrCorruptStorage)
	}
	return strconv.FormatUint(uint64(m.MapUID), 10), nil
}

func (u *userMailbox) OpenRecord(folder string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return nil, err
	}
	return u.Fetch(folder, name, m.AltTier)
}
