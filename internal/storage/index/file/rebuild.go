package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// ResetFolder atomically replaces the on-disk record set for
// folderID with the supplied messages. Preserves UIDVALIDITY,
// folder GUID and indexID; bumps NextUID past max(records.UID);
// HighestModSeq advances by one so QRESYNC clients invalidate
// their caches. After ResetFolder the .log file is empty and the
// .names sidecar is rewritten from scratch.
//
// Caller (admin rebuild flow) is responsible for having taken the
// cross-process mailbox lock before passing the records in.
func (u *userIndex) ResetFolder(folderID uint64, records []*mailbox.MessageMeta) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	return u.withMailboxLock(idx, func() error {
		if err := idx.rereadHeaderLocked(); err != nil {
			return err
		}
		idx.records = idx.records[:0]
		idx.filenames = make(map[uint32]string)
		idx.recKeys = make(map[uint32]uint32)
		idx.msgCount = 0
		idx.seenCount = 0
		idx.deletedCount = 0
		idx.modseq++ // QRESYNC: every Reset bumps modseq so caches invalidate

		var maxUID uint32
		for _, m := range records {
			if m == nil || m.UID == 0 {
				continue
			}
			kwBits, err := idx.internKeywords(m.Keywords)
			if err != nil {
				return fmt.Errorf("fileindex/reset: intern keywords for uid %d: %w", m.UID, err)
			}
			rec := indexRecord{
				uid:         m.UID,
				flags:       imapFlagsToIndex(m.Flags),
				modseq:      idx.modseq,
				keywordBits: kwBits,
			}
			idx.records = append(idx.records, rec)
			idx.msgCount++
			if rec.flags&flagSeen != 0 {
				idx.seenCount++
			}
			if rec.flags&flagDeleted != 0 {
				idx.deletedCount++
			}
			if kwBits != 0 {
				idx.recKeys[m.UID] = kwBits
			}
			if m.Filename != "" {
				idx.filenames[m.UID] = m.Filename
			}
			if m.UID > maxUID {
				maxUID = m.UID
			}
		}
		if maxUID >= idx.nextUID {
			idx.nextUID = maxUID + 1
		}
		if err := idx.rewriteBaseIndex(); err != nil {
			return err
		}
		if err := idx.resetLog(); err != nil {
			return err
		}
		return idx.rewriteNames()
	})
}

// OptimizeIndex compacts the .log overlay into the base .index
// file: rewrites .index from current in-memory state then truncates
// .log. No-op when the log is already empty (size <= header).
//
// Caller's mailbox lock is taken via withMailboxLock. Safe to call
// while no IMAP sessions reference this folder; concurrent readers
// see the lock and wait.
func (u *userIndex) OptimizeIndex(folderID uint64) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	return u.withMailboxLock(idx, func() error {
		if err := idx.rereadHeaderLocked(); err != nil {
			return err
		}
		// Skip when log carries only its own header.
		if st, err := os.Stat(idx.path + ".log"); err == nil && st.Size() <= 32 {
			return nil
		}
		if err := idx.rewriteBaseIndex(); err != nil {
			return err
		}
		return idx.resetLog()
	})
}

// rewriteBaseIndex truncates idx.path and writes a fresh
// [header | records...] dump using current in-memory state.
// Caller must hold both idx.mu and the cross-process mailbox lock.
func (idx *IndexFile) rewriteBaseIndex() error {
	tmp := idx.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex: open tmp %s: %w", tmp, err)
	}
	header := make([]byte, baseHeaderSize)
	le := binary.LittleEndian
	header[0] = indexMajor
	header[1] = indexMinor
	le.PutUint32(header[2:], uint32(baseHeaderSize))
	le.PutUint32(header[6:], uint32(baseHeaderSize))
	le.PutUint32(header[10:], idx.recordSize)
	header[14] = compatFlagsLE
	le.PutUint32(header[16:], idx.indexID)
	le.PutUint32(header[20:], 0)
	le.PutUint32(header[24:], idx.uidValidity)
	le.PutUint32(header[28:], idx.nextUID)
	le.PutUint32(header[32:], idx.msgCount)
	le.PutUint32(header[36:], idx.seenCount)
	le.PutUint32(header[40:], idx.deletedCount)
	// log offsets reset to defaults — resetLog runs immediately after.
	le.PutUint32(header[44:], idx.logFileSeq+1)
	le.PutUint32(header[48:], 0)
	le.PutUint32(header[52:], 0)
	le.PutUint64(header[56:], idx.modseq)
	copy(header[headerGUIDOffset:headerGUIDOffset+16], idx.guid[:])
	if _, err := f.Write(header); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex: write tmp header: %w", err)
	}

	recBuf := make([]byte, idx.recordSize)
	for _, rec := range idx.records {
		for i := range recBuf {
			recBuf[i] = 0
		}
		le.PutUint32(recBuf[0:], rec.uid)
		recBuf[4] = rec.flags
		le.PutUint64(recBuf[5:], rec.modseq)
		if idx.recordSize >= baseRecordSize+modseqExt+kwExt {
			le.PutUint32(recBuf[baseRecordSize+modseqExt:], rec.keywordBits)
		}
		if _, err := f.Write(recBuf); err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("fileindex: write tmp record uid=%d: %w", rec.uid, err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex: close tmp: %w", err)
	}
	if err := os.Rename(tmp, idx.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex: rename tmp: %w", err)
	}
	idx.logFileSeq++
	idx.logFileTail = 0
	idx.logFileHead = 0
	return nil
}

// resetLog closes the current log file, truncates .log to zero,
// and reopens with a fresh header. The log sequence number bumps
// so any reader that cached the old seq detects the reset.
func (idx *IndexFile) resetLog() error {
	if idx.logF != nil {
		_ = idx.logF.Close()
		idx.logF = nil
	}
	logPath := idx.path + ".log"
	if err := os.Truncate(logPath, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileindex: truncate log: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex: reopen log: %w", err)
	}
	if err := idx.writeLogHeader(f); err != nil {
		_ = f.Close()
		return err
	}
	idx.logF = f
	return nil
}

// rewriteNames truncates .names and rewrites it from idx.filenames.
// Used by ResetFolder so the on-disk filename map matches the new
// record set.
func (idx *IndexFile) rewriteNames() error {
	path := idx.path + ".names"
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex: open tmp names: %w", err)
	}
	for uid, name := range idx.filenames {
		if _, err := io.WriteString(f, fmt.Sprintf("%d\t%s\n", uid, name)); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("fileindex: write names: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex: close names: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex: rename names: %w", err)
	}
	return nil
}
