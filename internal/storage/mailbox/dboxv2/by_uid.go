package dboxv2

import (
	"fmt"
	"io"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An sdbox message is named by its uid, so the record is the whole answer and
// the folder needs no sidecar to say which file is whose (#1700).
func (u *userMailbox) RecordPath(_ string, m *mailbox.MessageMeta) (string, error) {
	if m.UID == 0 {
		return "", fmt.Errorf("sdbox/by-uid: uid 0 names no message")
	}
	return sdboxMailPrefix + strconv.FormatUint(uint64(m.UID), 10), nil
}

func (u *userMailbox) OpenRecord(folder string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return nil, err
	}
	return u.Fetch(folder, name, m.AltTier)
}
