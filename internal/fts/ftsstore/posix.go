// Package ftsstore builds the fts.IndexStore an engine writes its indexes
// through, from the driver named in fts_index_root.
package ftsstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// StorageTypeNFS is the declared medium whose metadata operations are committed
// by the protocol before the reply. Anything else is treated as a local
// filesystem, which is the safe direction: the extra fsync costs time, skipping
// one that was needed costs a wedged shard after a crash.
const StorageTypeNFS = "nfs"

// Posix keeps indexes as directories on a filesystem. It is the first
// implementation and the default one; fts_index_root names it as "posix".
type Posix struct {
	layout fts.Layout
	// storageType is what the operator declared the filesystem to be
	// (fts_storage_type). Whether a directory fsync buys anything is a
	// property of the medium, which is why the engine no longer asks.
	storageType string
}

// NewPosix returns a filesystem store for an engine's layout.
func NewPosix(layout fts.Layout, storageType string) *Posix {
	return &Posix{layout: layout, storageType: strings.ToLower(strings.TrimSpace(storageType))}
}

// dirSyncUseful reports whether fsyncing an index directory buys anything.
// Local filesystems: yes, it is what makes the rename and the removals survive
// a crash. NFS: no — the protocol commits metadata operations before the reply,
// and there is no commit-a-directory call to make, so the fsync is a no-op the
// kernel answers 0 to (#1176).
func (p *Posix) dirSyncUseful() bool { return p.storageType != StorageTypeNFS }

func (p *Posix) Locate(user fts.UserRef, mbox fts.MailboxRef) string {
	return p.layout.Dir(user.IndexRoot, user, mbox)
}

// Prepare resolves the location and moves an index found at an older layout to
// it, so switching layouts relocates an index in place instead of orphaning it
// and forcing a full reindex. Best-effort: on failure a fresh index is built at
// the current location (self-heals via autoindex). The yarilo-fts service is the
// sole writer, so no cross-process race.
func (p *Posix) Prepare(user fts.UserRef, mbox fts.MailboxRef) (string, error) {
	dir := p.Locate(user, mbox)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil // target already present — nothing to migrate
	}
	if p.layout.Legacy == nil {
		return dir, nil
	}
	var legacy string
	for _, cand := range p.layout.Legacy(user.IndexRoot, user, mbox) {
		if cand == dir {
			continue // a custom layout already yields this shape
		}
		if _, err := os.Stat(cand); err == nil {
			legacy = cand
			break
		}
	}
	if legacy == "" {
		return dir, nil // no older index — fresh mailbox
	}
	slog.Debug("fts/store: legacy dir migration starting", "from", legacy, "to", dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		slog.Warn("fts/store: legacy dir migration: mkdir parent", "from", legacy, "to", dir, "err", err)
		return dir, nil
	}
	if err := os.Rename(legacy, dir); err != nil {
		slog.Warn("fts/store: legacy dir migration failed; will reindex fresh",
			"from", legacy, "to", dir, "err", err)
		return dir, nil
	}
	pruneEmptyParents(filepath.Dir(legacy), user.IndexRoot)
	slog.Info("fts/store: migrated legacy FTS dir to the current layout", "from", legacy, "to", dir)
	return dir, nil
}

func (p *Posix) Create(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fts/store: mkdir %s: %w", dir, err)
	}
	return nil
}

// Sync fsyncs the directory so its entries (a freshly created or renamed shard)
// are durable — needed because the glass DB is opened with DB_NO_SYNC. On a
// medium that commits metadata by protocol the call is skipped, not issued.
func (p *Posix) Sync(dir string) error {
	if !p.dirSyncUseful() {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// pruneEmptyParents removes the directories a moved index leaves behind,
// climbing from dir while each is empty and strictly below root. os.Remove
// refuses a non-empty directory, which is the stop: a user whose other
// mailboxes are not migrated yet keeps the shell until they are.
//
// Not for the bytes — for the answer to "did the migration run". Left in place,
// <root>/mailboxes/<name> is present for every mailbox whether it moved or not,
// and the state that tells them apart is one level down (#1195). Best-effort:
// the data is already where it belongs.
func pruneEmptyParents(dir, root string) {
	root = filepath.Clean(root)
	for d := filepath.Clean(dir); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if err := os.Remove(d); err != nil {
			return
		}
	}
}
