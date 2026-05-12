// Package file implements the FileIndex IndexBackend.
// Wire format: .index (header + records), .index.log (transaction log),
// .index.cache (field cache). See INTERNALS.md §7.
package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Index file magic / version
const (
	indexMajor     = 7
	indexMinor     = 3
	logMajor       = 1
	logMinor       = 3
	baseHeaderSize = 120 // bytes
	baseRecordSize = 5   // uid(4) + flags(1)
	extRecordSize  = 8   // + modseq(8) — we always include modseq
	compatFlagsLE  = 0x01
)

// flag bits (IMAP standard flags stored in index record)
const (
	flagAnswered = 0x01
	flagFlagged  = 0x02
	flagDeleted  = 0x04
	flagSeen     = 0x08
	flagDraft    = 0x10
)

// transaction log record types
const (
	txExpunge      = 0x00000001
	txAppend       = 0x00000002
	txFlagUpdate   = 0x00000004
	txHeaderUpdate = 0x00000020
	txModseqUpdate = 0x00008000
	txBoundary     = 0x00080000
)

// IndexFile manages the .index file for one mailbox folder.
type IndexFile struct {
	mu   sync.Mutex
	path string // path to .index file (without extension suffix)

	// cached header values
	indexID      uint32
	uidValidity  uint32
	nextUID      uint32
	msgCount     uint32
	seenCount    uint32
	deletedCount uint32
	logFileSeq   uint32
	logFileTail  uint32
	logFileHead  uint32

	records []indexRecord
	logF    *os.File // append-only .index.log fd
	modseq  uint64
}

type indexRecord struct {
	uid    uint32
	flags  uint8
	modseq uint64
}

// folderID encodes user+folder into a uint64 for the IndexBackend interface.
// Format: not used directly — each IndexFile is opened per (user, folder).

// Backend is the FileIndex IndexBackend.
type Backend struct {
	root string
	mu   sync.Mutex
	open map[uint64]*IndexFile
	next uint64 // monotonically increasing folder ID counter
}

// New creates a FileIndex backend rooted at root.
func New(root string) *Backend {
	return &Backend{
		root: root,
		open: make(map[uint64]*IndexFile),
	}
}

func (b *Backend) OpenFolder(user, folder string, uidValidity uint32) (*mailbox.Folder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.next++
	id := b.next
	dir := b.indexDir(user, folder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	idx, err := openIndexFile(filepath.Join(dir, "dovecot.index"), uidValidity)
	if err != nil {
		return nil, err
	}
	b.open[id] = idx

	f := &mailbox.Folder{
		ID:            id,
		Name:          folder,
		UIDValidity:   idx.uidValidity,
		NextUID:       idx.nextUID,
		Messages:      idx.msgCount,
		Unseen:        idx.msgCount - idx.seenCount,
		HighestModSeq: idx.modseq,
	}
	return f, nil
}

func (b *Backend) SaveFolder(user string, f *mailbox.Folder) error {
	b.mu.Lock()
	idx, ok := b.open[f.ID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", f.ID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.nextUID = f.NextUID
	idx.msgCount = f.Messages
	return idx.writeHeader()
}

func (b *Backend) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	b.mu.Lock()
	idx, ok := b.open[folderID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rec := indexRecord{
		uid:    m.UID,
		flags:  imapFlagsToIndex(m.Flags),
		modseq: m.ModSeq,
	}
	idx.records = append(idx.records, rec)
	idx.msgCount++
	if rec.flags&flagSeen != 0 {
		idx.seenCount++
	}
	if rec.flags&flagDeleted != 0 {
		idx.deletedCount++
	}
	return idx.appendLogRecord(txAppend, rec)
}

func (b *Backend) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	b.mu.Lock()
	idx, ok := b.open[folderID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	newFlags := imapFlagsToIndex(flags)
	for i := range idx.records {
		if idx.records[i].uid != uid {
			continue
		}
		old := idx.records[i].flags
		idx.records[i].flags = newFlags
		idx.records[i].modseq = idx.modseq + 1
		idx.modseq++

		// maintain seen/deleted counters
		if old&flagSeen != 0 && newFlags&flagSeen == 0 {
			idx.seenCount--
		} else if old&flagSeen == 0 && newFlags&flagSeen != 0 {
			idx.seenCount++
		}
		if old&flagDeleted != 0 && newFlags&flagDeleted == 0 {
			idx.deletedCount--
		} else if old&flagDeleted == 0 && newFlags&flagDeleted != 0 {
			idx.deletedCount++
		}
		return idx.appendLogRecord(txFlagUpdate, idx.records[i])
	}
	return nil
}

func (b *Backend) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	b.mu.Lock()
	idx, ok := b.open[folderID]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var result []*mailbox.MessageMeta
	for _, rec := range idx.records {
		if seqSetContains(uids, rec.uid) {
			result = append(result, &mailbox.MessageMeta{
				UID:    rec.uid,
				Flags:  indexFlagsToIMAP(rec.flags),
				ModSeq: rec.modseq,
			})
		}
	}
	return result, nil
}

func (b *Backend) ExpungeMessage(folderID uint64, uid uint32) error {
	b.mu.Lock()
	idx, ok := b.open[folderID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, rec := range idx.records {
		if rec.uid != uid {
			continue
		}
		if rec.flags&flagSeen != 0 {
			idx.seenCount--
		}
		if rec.flags&flagDeleted != 0 {
			idx.deletedCount--
		}
		idx.records = append(idx.records[:i], idx.records[i+1:]...)
		idx.msgCount--
		return idx.appendLogRecord(txExpunge, rec)
	}
	return nil
}

func (b *Backend) NextModSeq(folderID uint64) (uint64, error) {
	b.mu.Lock()
	idx, ok := b.open[folderID]
	b.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.modseq++
	return idx.modseq, nil
}

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, idx := range b.open {
		if err := idx.close(); err != nil {
			return err
		}
	}
	b.open = make(map[uint64]*IndexFile)
	return nil
}

func (b *Backend) indexDir(user, folder string) string {
	if i := strings.LastIndex(user, "@"); i >= 0 {
		domain := user[i+1:]
		local := user[:i]
		return filepath.Join(b.root, domain, local, "."+folder)
	}
	return filepath.Join(b.root, user, "."+folder)
}

// ---- IndexFile low-level ------------------------------------------------

func openIndexFile(path string, uidValidity uint32) (*IndexFile, error) {
	idx := &IndexFile{path: path}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return idx.initNew(uidValidity)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if err := idx.readHeader(f); err != nil {
		return nil, fmt.Errorf("fileindex: read header %s: %w", path, err)
	}
	if err := idx.readRecords(f); err != nil {
		return nil, fmt.Errorf("fileindex: read records %s: %w", path, err)
	}
	if err := idx.replayLog(); err != nil {
		return nil, fmt.Errorf("fileindex: replay log %s: %w", path, err)
	}
	if err := idx.openLog(); err != nil {
		return nil, err
	}
	return idx, nil
}

func (idx *IndexFile) initNew(uidValidity uint32) (*IndexFile, error) {
	idx.indexID = uint32(time.Now().Unix())
	idx.uidValidity = uidValidity
	idx.nextUID = 1
	idx.modseq = 1

	if err := idx.writeHeader(); err != nil {
		return nil, err
	}
	if err := idx.openLog(); err != nil {
		return nil, err
	}
	return idx, nil
}

// writeHeader writes the 120-byte index header to the .index file.
func (idx *IndexFile) writeHeader() error {
	buf := make([]byte, baseHeaderSize)
	le := binary.LittleEndian

	buf[0] = indexMajor
	buf[1] = indexMinor
	le.PutUint32(buf[2:], uint32(baseHeaderSize))
	le.PutUint32(buf[6:], uint32(baseHeaderSize))
	le.PutUint32(buf[10:], uint32(baseRecordSize+extRecordSize))
	buf[14] = compatFlagsLE

	le.PutUint32(buf[16:], idx.indexID)
	le.PutUint32(buf[20:], 0) // flags
	le.PutUint32(buf[24:], idx.uidValidity)
	le.PutUint32(buf[28:], idx.nextUID)
	le.PutUint32(buf[32:], idx.msgCount)
	le.PutUint32(buf[36:], idx.seenCount)
	le.PutUint32(buf[40:], idx.deletedCount)
	le.PutUint32(buf[44:], idx.logFileSeq)
	le.PutUint32(buf[48:], idx.logFileTail)
	le.PutUint32(buf[52:], idx.logFileHead)
	binary.LittleEndian.PutUint64(buf[56:], idx.modseq)

	f, err := os.OpenFile(idx.path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt(buf, 0)
	return err
}

func (idx *IndexFile) readHeader(f *os.File) error {
	buf := make([]byte, baseHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	le := binary.LittleEndian
	if buf[0] != indexMajor {
		return fmt.Errorf("fileindex: major version mismatch: got %d want %d", buf[0], indexMajor)
	}
	idx.indexID = le.Uint32(buf[16:])
	idx.uidValidity = le.Uint32(buf[24:])
	idx.nextUID = le.Uint32(buf[28:])
	idx.msgCount = le.Uint32(buf[32:])
	idx.seenCount = le.Uint32(buf[36:])
	idx.deletedCount = le.Uint32(buf[40:])
	idx.logFileSeq = le.Uint32(buf[44:])
	idx.logFileTail = le.Uint32(buf[48:])
	idx.logFileHead = le.Uint32(buf[52:])
	idx.modseq = le.Uint64(buf[56:])
	return nil
}

func (idx *IndexFile) readRecords(f *os.File) error {
	recSize := baseRecordSize + extRecordSize
	buf := make([]byte, recSize)
	for {
		_, err := io.ReadFull(f, buf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
		rec := indexRecord{
			uid:    binary.LittleEndian.Uint32(buf[0:]),
			flags:  buf[4],
			modseq: binary.LittleEndian.Uint64(buf[5:]),
		}
		if rec.uid == 0 {
			continue
		}
		idx.records = append(idx.records, rec)
	}
	return nil
}

// replayLog applies committed transactions from .index.log.
func (idx *IndexFile) replayLog() error {
	logPath := idx.path + ".log"
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	// Skip 32-byte log header
	if _, err := f.Seek(32, io.SeekStart); err != nil {
		return nil
	}

	buf4 := make([]byte, 4)
	for {
		if _, err := io.ReadFull(f, buf4); err != nil {
			break
		}
		size := binary.LittleEndian.Uint32(buf4)
		if size < 8 {
			break
		}
		rec := make([]byte, size-4)
		if _, err := io.ReadFull(f, rec); err != nil {
			break
		}
		txType := binary.LittleEndian.Uint32(rec[:4])
		data := rec[4:]
		idx.applyLogRecord(txType, data)
	}
	return nil
}

func (idx *IndexFile) applyLogRecord(txType uint32, data []byte) {
	switch txType {
	case txAppend:
		if len(data) < 13 {
			return
		}
		rec := indexRecord{
			uid:    binary.LittleEndian.Uint32(data[0:]),
			flags:  data[4],
			modseq: binary.LittleEndian.Uint64(data[5:]),
		}
		idx.records = append(idx.records, rec)
		idx.msgCount++
	case txExpunge:
		if len(data) < 4 {
			return
		}
		uid := binary.LittleEndian.Uint32(data[0:])
		for i, r := range idx.records {
			if r.uid == uid {
				idx.records = append(idx.records[:i], idx.records[i+1:]...)
				idx.msgCount--
				break
			}
		}
	case txFlagUpdate:
		if len(data) < 13 {
			return
		}
		uid := binary.LittleEndian.Uint32(data[0:])
		newFlags := data[4]
		newModSeq := binary.LittleEndian.Uint64(data[5:])
		for i := range idx.records {
			if idx.records[i].uid == uid {
				idx.records[i].flags = newFlags
				idx.records[i].modseq = newModSeq
				break
			}
		}
	}
}

// openLog opens or creates the append-only .index.log file.
func (idx *IndexFile) openLog() error {
	logPath := idx.path + ".log"
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex: open log: %w", err)
	}
	info, _ := f.Stat()
	if info != nil && info.Size() == 0 {
		if err := idx.writeLogHeader(f); err != nil {
			f.Close()
			return err
		}
	}
	idx.logF = f
	return nil
}

// writeLogHeader writes the 32-byte log file header.
func (idx *IndexFile) writeLogHeader(f *os.File) error {
	buf := make([]byte, 32)
	le := binary.LittleEndian
	buf[0] = logMajor
	buf[1] = logMinor
	le.PutUint32(buf[2:], 32) // hdr_size
	le.PutUint32(buf[6:], idx.indexID)
	le.PutUint32(buf[10:], idx.logFileSeq)
	binary.LittleEndian.PutUint64(buf[18:], idx.modseq) // initial_modseq
	le.PutUint32(buf[26:], uint32(time.Now().Unix()))   // create_stamp
	_, err := f.Write(buf)
	return err
}

// appendLogRecord writes one transaction record to .index.log.
// Format: size(4) + type(4) + uid(4) + flags(1) + modseq(8) = 21 bytes total.
func (idx *IndexFile) appendLogRecord(txType uint32, rec indexRecord) error {
	if idx.logF == nil {
		return nil
	}
	const payloadSize = 13 // uid(4)+flags(1)+modseq(8)
	const totalSize = 4 + 4 + payloadSize
	buf := make([]byte, totalSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], totalSize)
	le.PutUint32(buf[4:], txType)
	le.PutUint32(buf[8:], rec.uid)
	buf[12] = rec.flags
	le.PutUint64(buf[13:], rec.modseq)
	_, err := idx.logF.Write(buf)
	return err
}

func (idx *IndexFile) close() error {
	if idx.logF != nil {
		return idx.logF.Close()
	}
	return nil
}

// seqSetContains reports whether uid is in the set.
// An empty set or a range with From=0,To=0 matches everything.
func seqSetContains(s mailbox.SeqSet, uid uint32) bool {
	if len(s) == 0 {
		return true
	}
	for _, r := range s {
		if r.From == 0 && r.To == 0 {
			return true
		}
		hi := r.To
		if hi == 0 {
			hi = ^uint32(0)
		}
		if uid >= r.From && uid <= hi {
			return true
		}
	}
	return false
}

// ---- flag conversion ---------------------------------------------------

func imapFlagsToIndex(flags []string) uint8 {
	var b uint8
	for _, f := range flags {
		switch f {
		case `\Answered`:
			b |= flagAnswered
		case `\Flagged`:
			b |= flagFlagged
		case `\Deleted`:
			b |= flagDeleted
		case `\Seen`:
			b |= flagSeen
		case `\Draft`:
			b |= flagDraft
		}
	}
	return b
}

func indexFlagsToIMAP(b uint8) []string {
	var flags []string
	if b&flagAnswered != 0 {
		flags = append(flags, `\Answered`)
	}
	if b&flagFlagged != 0 {
		flags = append(flags, `\Flagged`)
	}
	if b&flagDeleted != 0 {
		flags = append(flags, `\Deleted`)
	}
	if b&flagSeen != 0 {
		flags = append(flags, `\Seen`)
	}
	if b&flagDraft != 0 {
		flags = append(flags, `\Draft`)
	}
	return flags
}
