package mailbox

// RecordSaved allocates the uid and records the message, letting a driver named
// by uid settle the name in that cycle. m.Filename: in what Save returned, out
// the name kept (#1704).
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

// NameSaved names a saved message when the caller already holds the uid, as a
// delivery that reserved one does. No cycle of its own.
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
