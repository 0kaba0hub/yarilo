package mailbox

// RecordSaved allocates the uid and records the message; a driver named by uid
// settles the name in that cycle. m.Filename: in what Save gave, out what it keeps.
func RecordSaved(idx UserIndex, box UserMailbox, folderID uint64, folder string, m *MessageMeta) error {
	stampStorageKey(box, folder, m)
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
	defer stampStorageKey(box, folder, m)
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

// stampStorageKey carries a driver's own key into the record, where it belongs:
// mdbox's map_uid, which the name is read back from.
func stampStorageKey(box UserMailbox, folder string, m *MessageMeta) {
	keyer, ok := Driver(box).(StorageKeyer)
	if !ok {
		return
	}
	if mapUID, saveDate, have := keyer.StorageKey(folder, m.Filename); have {
		m.MapUID, m.SaveDate = mapUID, saveDate
	}
}
