package imap

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// uidNameMigrator is a driver holding messages under names it wrote before the
// name became the uid's (#1704).
type uidNameMigrator interface {
	MigrateUIDNames(mailbox.UserIndex, *mailbox.Folder) (int, error)
}

// migrateNamesOnSelect brings a folder's file names to u.<uid> before a SELECT
// builds its view, and returns a refreshed handle when anything moved.
func (s *session) migrateNamesOnSelect(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	m, ok := mailbox.Driver(h.box).(uidNameMigrator)
	if !ok {
		return nil
	}
	n, err := m.MigrateUIDNames(h.idx, f)
	if err != nil {
		slog.Warn("imap: name migration failed", "user", s.username(), "folder", rel, "err", err)
		return nil
	}
	if n == 0 {
		return nil
	}
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after name migration failed", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}
