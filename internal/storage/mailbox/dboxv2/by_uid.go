package dboxv2

import (
	"fmt"
	"io"
	"strconv"
)

// OpenByUID and PathByUID: an sdbox message is named by its uid, so the folder
// needs no sidecar to say which file is whose (#1700).
func (u *userMailbox) PathByUID(_ string, uid uint32) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("sdbox/by-uid: uid 0 names no message")
	}
	return sdboxMailPrefix + strconv.FormatUint(uint64(uid), 10), nil
}

func (u *userMailbox) OpenByUID(folder string, uid uint32, altTier bool) (io.ReadCloser, error) {
	name, err := u.PathByUID(folder, uid)
	if err != nil {
		return nil, err
	}
	return u.Fetch(folder, name, altTier)
}
