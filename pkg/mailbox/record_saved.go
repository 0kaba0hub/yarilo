package mailbox

// RecordSaved allocates the message's uid and records it, letting a driver
// whose file name is the uid settle that name inside the same cycle: a second
// cycle would take the folder key twice for one save (#1704).
//
// m.Filename must hold what Save returned. On return it holds the name the
// message keeps.
func RecordSaved(idx UserIndex, box UserMailbox, folderID uint64, folder string, m *MessageMeta) error {
	namer, isNamer := Driver(box).(UIDNamer)
	appender, isAppender := idx.(NamingAppender)
	if !isNamer || !isAppender {
		return idx.AllocateAndAppend(folderID, m)
	}
	saved := m.Filename
	return appender.AllocateAndAppendNamed(folderID, m, func(uid uint32) (string, error) {
		return namer.AssignUID(folder, saved, uid)
	})
}

// NameSaved gives a saved message its final name when the caller already holds
// the uid, as a delivery that reserved one does. No cycle of its own: the
// reservation and the append are the caller's.
func NameSaved(box UserMailbox, folder string, m *MessageMeta) error {
	namer, ok := Driver(box).(UIDNamer)
	if !ok {
		return nil
	}
	named, err := namer.AssignUID(folder, m.Filename, m.UID)
	if err != nil {
		return err
	}
	m.Filename = named
	return nil
}
