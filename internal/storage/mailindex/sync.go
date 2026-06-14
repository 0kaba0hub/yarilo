package mailindex

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// tmpSeq is a process-wide atomic counter used to disambiguate
// .tmp.<pid>.<seq> filenames. Per-process PID is not enough when
// multiple Recreate callers in the same Go process run
// concurrently (one userIndex per simulated process in
// integration tests, both with same PID).
var tmpSeq atomic.Uint64

// RecreateInput bundles everything Recreate needs in one struct
// so call sites stay readable. All fields except Header and Path
// are optional.
//
//   - Path is the canonical .index file path; the tmp file lives
//     alongside as "<Path>.tmp.<pid>" and is renamed in place
//     after every byte is flushed.
//   - Header is the freshly-encoded base header (its
//     log_file_tail_offset MUST equal log_file_head_offset per
//     the rewrite invariant — the caller does that bookkeeping
//     before calling).
//   - Extensions is the ordered slice of registered extensions
//     whose layout produced Header.RecordSize and whose entries
//     are written into the extended-header region.
//   - Records is the in-memory image of all live records. Their
//     order on disk follows the slice order; callers usually
//     pass them sorted ascending by UID for deterministic output.
//   - KeepBackup, when true, hard-links the OLD .index to
//     "<Path>.backup" before rename. Mirrors
//     MAIL_INDEX_OPEN_FLAG_KEEP_BACKUPS.
//   - Fsync, when true, fsync()s the tmp file before rename so a
//     crash between rename and the next fsync still leaves the
//     new bytes on disk.
type RecreateInput struct {
	Path       string
	Header     Header
	Extensions []Extension
	Records    []*Record
	KeepBackup bool
	Fsync      bool
	// TmpDir, when non-empty, redirects the write-side tmp file to a
	// separate directory (e.g. a local tmpfs). The fsync cost then
	// lands on local storage instead of NFS. After the tmp is closed
	// the file is copied to a staging path next to the target and
	// renamed into place atomically — no cross-device rename occurs.
	TmpDir string
}

// Recreate writes the base index file at Path atomically: open
// <Path>.tmp.<pid>, write header + extended header + records,
// optionally fsync, optionally hardlink old to .backup, then
// rename tmp into place.
//
// Caller MUST hold the cross-process write lock on this index
// before calling — Recreate does not take any locks of its own.
// The recommended pattern is to call Recreate from inside
// WithSyncLock so the lock release and rename ordering stay
// correct.
//
// Returns the path of the .backup hardlink when KeepBackup is
// true (empty string otherwise). On any error the tmp file is
// removed and the on-disk .index is unchanged.
func Recreate(in RecreateInput) (backupPath string, err error) {
	if in.Path == "" {
		return "", fmt.Errorf("mailindex: Recreate: empty Path")
	}
	layout, err := ComputeRecordLayout(in.Extensions)
	if err != nil {
		return "", fmt.Errorf("mailindex/recreate: layout: %w", err)
	}
	if layout.RecordSize != in.Header.RecordSize {
		return "", fmt.Errorf("mailindex/recreate: layout.RecordSize=%d, header.RecordSize=%d: %w",
			layout.RecordSize, in.Header.RecordSize, ErrCorrupted)
	}
	extBytes, err := EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return "", fmt.Errorf("mailindex/recreate: encode ext headers: %w", err)
	}
	expectedHeaderSize := uint32(HeaderMinSize) + uint32(len(extBytes))
	if in.Header.HeaderSize != expectedHeaderSize {
		return "", fmt.Errorf("mailindex/recreate: header.HeaderSize=%d but base+ext=%d: %w",
			in.Header.HeaderSize, expectedHeaderSize, ErrCorrupted)
	}

	var tmpPath string
	if in.TmpDir != "" {
		tmpPath = filepath.Join(in.TmpDir, filepath.Base(in.Path)+fmt.Sprintf(".tmp.%d.%d", os.Getpid(), tmpSeq.Add(1)))
	} else {
		tmpPath = fmt.Sprintf("%s.tmp.%d.%d", in.Path, os.Getpid(), tmpSeq.Add(1))
	}
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("mailindex/recreate: open tmp: %w", err)
	}
	cleanup := func(failed bool) {
		_ = f.Close()
		if failed {
			_ = os.Remove(tmpPath)
		}
	}
	if _, err := f.Write(in.Header.EncodeBytes()); err != nil {
		cleanup(true)
		return "", fmt.Errorf("mailindex/recreate: write header: %w", err)
	}
	if len(extBytes) > 0 {
		if _, err := f.Write(extBytes); err != nil {
			cleanup(true)
			return "", fmt.Errorf("mailindex/recreate: write ext headers: %w", err)
		}
	}
	if layout.RecordSize > 0 && len(in.Records) > 0 {
		stride := int(layout.RecordSize)
		buf := make([]byte, stride)
		for i, rec := range in.Records {
			if err := EncodeRecord(buf, layout, rec); err != nil {
				cleanup(true)
				return "", fmt.Errorf("mailindex/recreate: encode rec %d: %w", i, err)
			}
			if _, err := f.Write(buf); err != nil {
				cleanup(true)
				return "", fmt.Errorf("mailindex/recreate: write rec %d: %w", i, err)
			}
		}
	}
	if in.Fsync {
		if err := f.Sync(); err != nil {
			cleanup(true)
			return "", fmt.Errorf("mailindex/recreate: fsync: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("mailindex/recreate: close: %w", err)
	}

	// When TmpDir is set the tmp lives on a different filesystem; copy it
	// to a staging path next to the target so the final rename is atomic
	// and same-device.
	commitPath := tmpPath
	if in.TmpDir != "" {
		stagePath := fmt.Sprintf("%s.stage.%d.%d", in.Path, os.Getpid(), tmpSeq.Add(1))
		if err := copyTmp(tmpPath, stagePath); err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("mailindex/recreate: cross-device stage: %w", err)
		}
		_ = os.Remove(tmpPath)
		commitPath = stagePath
	}

	if in.KeepBackup {
		backupPath = in.Path + ".backup"
		// Remove a stale backup so Link doesn't fail with EEXIST.
		_ = os.Remove(backupPath)
		if _, statErr := os.Stat(in.Path); statErr == nil {
			if err := os.Link(in.Path, backupPath); err != nil {
				_ = os.Remove(commitPath)
				return "", fmt.Errorf("mailindex/recreate: hardlink backup: %w", err)
			}
		}
	}
	if err := os.Rename(commitPath, in.Path); err != nil {
		_ = os.Remove(commitPath)
		return "", fmt.Errorf("mailindex/recreate: rename: %w", err)
	}
	return backupPath, nil
}

// copyTmp copies src to dst with mode 0o600. Used when the tmp file lives on
// a different filesystem than the target and os.Rename would fail with EXDEV.
func copyTmp(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// SyncLockKey returns the locks resource key for a given index
// path. Convention: "mailindex:<absolute-path>". Caller passes
// the result to pkg/locks.Acquire — having the path in the key
// means concurrent readers of different files do not contend.
func SyncLockKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "mailindex:" + abs
}

// WithSyncLock runs fn under the cross-process write lock for
// the given index path. The lock is held for ttl (renewal is
// up to fn — typical sync operations finish well within ttl so
// it's not needed). owner is shown in BUSY reports from the
// lock server; the convention "<process>/<pid>/<context>" is
// recommended.
//
// Returns the error from Acquire, fn, or Unlock — Acquire
// errors propagate immediately; Unlock errors are wrapped after
// fn completes.
func WithSyncLock(ctx context.Context, l locks.Locker, path, owner string, ttl time.Duration, fn func() error) error {
	if l == nil {
		// No locker wired (unit tests / dev CLI) — caller has
		// already accepted single-process-only correctness.
		return fn()
	}
	key := SyncLockKey(path)
	lk, err := locks.Acquire(ctx, l, key, owner, ttl)
	if err != nil {
		return fmt.Errorf("mailindex/sync: acquire %s: %w", key, err)
	}
	fnErr := fn()
	if err := l.Unlock(context.Background(), lk.ID); err != nil && fnErr == nil {
		return fmt.Errorf("mailindex/sync: unlock %s: %w", key, err)
	}
	return fnErr
}
