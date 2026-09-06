package dboxv2

import (
	"strings"
	"time"

	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// MigrateUIDNames renames every message this server stored under a name derived
// from its GUID to the only name the reference knows, u.<uid> (#1704). One lock
// cycle for the folder; the index is corrected in one batch after it.
func (u *userMailbox) MigrateUIDNames(idx mailbox.UserIndex, folder *mailbox.Folder) (int, error) {
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return 0, fmt.Errorf("sdbox/migrate: get messages %q: %w", folder.Name, err)
	}
	renamed := make(map[uint32]string, len(msgs))
	err = u.withMailboxLock(folder.Name, func() error {
		dir := u.folderPath(folder.Name)
		if serr := sweepStaleTemps(dir); serr != nil {
			return serr
		}
		for _, m := range msgs {
			want := sdboxMailPrefix + strconv.FormatUint(uint64(m.UID), 10)
			if m.Filename == "" || m.Filename == want {
				continue
			}
			from := filepath.Join(dir, m.Filename)
			if _, serr := os.Lstat(from); serr != nil {
				// Nothing to rename: a record whose file is already gone is the
				// reactive heal's business, not this pass's.
				continue
			}
			if err := os.Rename(from, filepath.Join(dir, want)); err != nil {
				return fmt.Errorf("sdbox/migrate: rename %s -> %s: %w", m.Filename, want, err)
			}
			renamed[m.UID] = want
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(renamed) == 0 {
		return 0, nil
	}
	// After the rename, never before: a crash between the two leaves a file the
	// index cannot name, and the next pass renames nothing and repairs it.
	writer, ok := idx.(mailbox.FilenameWriterMulti)
	if !ok {
		return 0, fmt.Errorf("sdbox/migrate: %q: the index cannot record a batch of names", folder.Name)
	}
	if err := writer.UpdateFilenames(folder.ID, renamed); err != nil {
		return 0, fmt.Errorf("sdbox/migrate: record names %q: %w", folder.Name, err)
	}
	slog.Info("sdbox: renamed messages to the name their uid gives them",
		"user", u.username, "folder", folder.Name, "renamed", len(renamed))
	return len(renamed), nil
}

// staleTemp is when a half-finished save stops being one: a save names its file
// within a cycle, so anything this old is a crash's leftover.
const staleTemp = 24 * time.Hour

// sweepStaleTemps removes those leftovers. A young one is left alone: it is a
// save in flight, and its own caller is about to name it.
func sweepStaleTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sdbox/migrate: list %s: %w", dir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), temporaryPrefix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < staleTemp {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("sdbox/migrate: remove %s: %w", e.Name(), rerr)
		}
		slog.Info("sdbox: removed a save that never got a uid", "file", e.Name(), "dir", dir)
	}
	return nil
}
