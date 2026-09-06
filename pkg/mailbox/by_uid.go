package mailbox

import "io"

// OpenMessage reads a message's body. A driver that can find it from the uid is
// asked that way; the rest are handed the name the record carries (#1700).
func OpenMessage(box UserMailbox, folder string, m *MessageMeta) (io.ReadCloser, error) {
	if addr, ok := Driver(box).(UIDAddressable); ok {
		return addr.OpenByUID(folder, m.UID, m.AltTier)
	}
	return box.Fetch(folder, m.Filename, m.AltTier)
}

// MessagePath names a message for the operations that take a name -- copy,
// move, remove. Same rule: derived where it can be, carried where it cannot.
func MessagePath(box UserMailbox, folder string, m *MessageMeta) (string, error) {
	if addr, ok := Driver(box).(UIDAddressable); ok {
		return addr.PathByUID(folder, m.UID)
	}
	return m.Filename, nil
}

// RemoveMessage unlinks it, by whichever of the two the driver understands.
func RemoveMessage(box UserMailbox, folder string, m *MessageMeta) error {
	name, err := MessagePath(box, folder, m)
	if err != nil {
		return err
	}
	return box.Remove(folder, name)
}
