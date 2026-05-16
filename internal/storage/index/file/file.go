// Package file implements the FileIndex IndexBackend.
// Wire format: .index (header + records), .index.log (transaction log),
// .index.cache (field cache). See INTERNALS.md §7.
package file

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	baseHeaderSize = 120
	baseRecordSize = 5 // uid(4) + flags(1)
	modseqExt      = 8 // modseq extension: always present
	kwExt          = 4 // keyword bitmask extension: 4 bytes = 32 keywords
	compatFlagsLE  = 0x01

	maxKeywords = 32
)

// flag bits
const (
	flagAnswered = 0x01
	flagFlagged  = 0x02
	flagDeleted  = 0x04
	flagSeen     = 0x08
	flagDraft    = 0x10
)

// transaction log record types
const (
	txExpunge       = 0x00000001
	txAppend        = 0x00000002
	txFlagUpdate    = 0x00000004
	txHeaderUpdate  = 0x00000020
	txExtIntro      = 0x00000040
	txKeywordUpdate = 0x00000400
	txModseqUpdate  = 0x00008000
	txBoundary      = 0x00080000
)

// IndexFile manages the .index file for one mailbox folder.
type IndexFile struct {
	mu   sync.Mutex
	path string

	indexID      uint32
	uidValidity  uint32
	nextUID      uint32
	msgCount     uint32
	seenCount    uint32
	deletedCount uint32
	logFileSeq   uint32
	logFileTail  uint32
	logFileHead  uint32
	recordSize   uint32

	records   []indexRecord
	filenames map[uint32]string // uid → backend filename
	recKeys   map[uint32]uint32 // uid → keyword bitmask
	keywords  []string
	logF      *os.File
	modseq    uint64
}

type indexRecord struct {
	uid         uint32
	flags       uint8
	modseq      uint64
	keywordBits uint32
}

// Backend is the FileIndex IndexBackend factory.
// It holds no per-user state.
type Backend struct{}

// New creates a FileIndex backend.
func New() *Backend { return &Backend{} }

// OpenUser returns a per-session index handle bound to u.
// The index files are stored under u.Home (one .index file per folder).
func (b *Backend) OpenUser(u *mailbox.UserInfo) mailbox.UserIndex {
	return &userIndex{
		home: u.Home,
		open: make(map[uint64]*IndexFile),
	}
}

// userIndex is a per-session, per-user index handle.
// Folder IDs assigned here are local to this handle.
type userIndex struct {
	home string
	mu   sync.Mutex
	open map[uint64]*IndexFile
	next uint64
}

func (u *userIndex) OpenFolder(folder string, uidValidity uint32) (*mailbox.Folder, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.next++
	id := u.next
	dir := u.indexDir(folder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	idx, err := openIndexFile(filepath.Join(dir, "dovecot.index"), uidValidity)
	if err != nil {
		return nil, err
	}
	u.open[id] = idx

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

func (u *userIndex) SaveFolder(f *mailbox.Folder) error {
	u.mu.Lock()
	idx, ok := u.open[f.ID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", f.ID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.nextUID = f.NextUID
	idx.msgCount = f.Messages
	return idx.writeHeader()
}

func (u *userIndex) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	kwBits, err := idx.internKeywords(m.Keywords)
	if err != nil {
		return err
	}

	rec := indexRecord{
		uid:         m.UID,
		flags:       imapFlagsToIndex(m.Flags),
		modseq:      m.ModSeq,
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
		if err := idx.appendNameEntry(m.UID, m.Filename); err != nil {
			return err
		}
	}
	if err := idx.appendLogRecord(txAppend, rec); err != nil {
		return err
	}
	if kwBits != 0 {
		return idx.appendKeywordUpdateLog(m.UID, kwBits)
	}
	return nil
}

func (u *userIndex) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	kwBits, err := idx.internKeywords(keywords)
	if err != nil {
		return err
	}

	newFlags := imapFlagsToIndex(flags)
	for i := range idx.records {
		if idx.records[i].uid != uid {
			continue
		}
		old := idx.records[i].flags
		idx.records[i].flags = newFlags
		idx.records[i].modseq = idx.modseq + 1
		idx.records[i].keywordBits = kwBits
		idx.modseq++

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
		if kwBits != 0 {
			idx.recKeys[uid] = kwBits
		} else {
			delete(idx.recKeys, uid)
		}
		if err := idx.appendLogRecord(txFlagUpdate, idx.records[i]); err != nil {
			return err
		}
		if kwBits != 0 {
			return idx.appendKeywordUpdateLog(uid, kwBits)
		}
		return nil
	}
	return nil
}

func (u *userIndex) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var result []*mailbox.MessageMeta
	for _, rec := range idx.records {
		if seqSetContains(uids, rec.uid) {
			result = append(result, &mailbox.MessageMeta{
				UID:      rec.uid,
				Filename: idx.filenames[rec.uid],
				Flags:    indexFlagsToIMAP(rec.flags),
				Keywords: idx.bitsToKeywords(rec.keywordBits),
				ModSeq:   rec.modseq,
			})
		}
	}
	return result, nil
}

func (u *userIndex) ExpungeMessage(folderID uint64, uid uint32) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
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
		delete(idx.recKeys, uid)
		return idx.appendLogRecord(txExpunge, rec)
	}
	return nil
}

func (u *userIndex) NextModSeq(folderID uint64) (uint64, error) {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.modseq++
	return idx.modseq, nil
}

func (u *userIndex) Keywords(folderID uint64) ([]string, error) {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]string, len(idx.keywords))
	copy(out, idx.keywords)
	return out, nil
}

func (u *userIndex) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, idx := range u.open {
		if err := idx.close(); err != nil {
			return err
		}
	}
	u.open = make(map[uint64]*IndexFile)
	return nil
}

func (u *userIndex) indexDir(folder string) string {
	return filepath.Join(u.home, "."+folder)
}

// RenameFolder renames the on-disk index directory for a folder.
// Any open IndexFile handles remain valid after the rename (file descriptors
// survive directory renames on POSIX filesystems).
func (u *userIndex) RenameFolder(oldName, newName string) error {
	if err := os.Rename(u.indexDir(oldName), u.indexDir(newName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileindex/rename: %w", err)
	}
	return nil
}

// GetPOP3UIDLs reads the pop3.uidl file for the folder.
// Each line is "<uid>\t<uidl>". Returns empty map when file does not exist.
func (u *userIndex) GetPOP3UIDLs(folderID uint64) (map[uint32]string, error) {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	path := filepath.Join(filepath.Dir(idx.path), "pop3.uidl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[uint32]string), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	result := make(map[uint32]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		uid, err := strconv.ParseUint(line[:tab], 10, 32)
		if err != nil {
			continue
		}
		result[uint32(uid)] = line[tab+1:]
	}
	return result, sc.Err()
}

// SavePOP3UIDLs writes uid→uidl pairs to pop3.uidl atomically (write-tmp + rename).
func (u *userIndex) SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error {
	u.mu.Lock()
	idx, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	dir := filepath.Dir(idx.path)
	dst := filepath.Join(dir, "pop3.uidl")
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("pop3.uidl write: %w", err)
	}
	bw := bufio.NewWriter(f)
	for uid, uidl := range uidls {
		fmt.Fprintf(bw, "%d\t%s\n", uid, uidl) //nolint:errcheck
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("pop3.uidl flush: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("pop3.uidl close: %w", err)
	}
	return os.Rename(tmp, dst)
}

// ---- IndexFile low-level ---------------------------------------------------

func openIndexFile(path string, uidValidity uint32) (*IndexFile, error) {
	idx := &IndexFile{
		path:      path,
		filenames: make(map[uint32]string),
		recKeys:   make(map[uint32]uint32),
	}

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
	idx.loadKeywords()
	idx.loadNames()
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
	idx.recordSize = baseRecordSize + modseqExt

	if err := idx.writeHeader(); err != nil {
		return nil, err
	}
	if err := idx.openLog(); err != nil {
		return nil, err
	}
	return idx, nil
}

func (idx *IndexFile) writeHeader() error {
	buf := make([]byte, baseHeaderSize)
	le := binary.LittleEndian

	buf[0] = indexMajor
	buf[1] = indexMinor
	le.PutUint32(buf[2:], uint32(baseHeaderSize))
	le.PutUint32(buf[6:], uint32(baseHeaderSize))
	le.PutUint32(buf[10:], idx.recordSize)
	buf[14] = compatFlagsLE

	le.PutUint32(buf[16:], idx.indexID)
	le.PutUint32(buf[20:], 0)
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
	idx.recordSize = le.Uint32(buf[10:])
	if idx.recordSize < baseRecordSize+modseqExt {
		idx.recordSize = baseRecordSize + modseqExt
	}
	return nil
}

func (idx *IndexFile) readRecords(f *os.File) error {
	rs := idx.recordSize
	buf := make([]byte, rs)
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
		if rs >= baseRecordSize+modseqExt+kwExt {
			rec.keywordBits = binary.LittleEndian.Uint32(buf[baseRecordSize+modseqExt:])
		}
		if rec.uid == 0 {
			continue
		}
		idx.records = append(idx.records, rec)
		if rec.keywordBits != 0 {
			idx.recKeys[rec.uid] = rec.keywordBits
		}
	}
	return nil
}

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
		if len(data) >= 17 {
			rec.keywordBits = binary.LittleEndian.Uint32(data[13:])
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
				delete(idx.recKeys, uid)
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
		var kwBits uint32
		if len(data) >= 17 {
			kwBits = binary.LittleEndian.Uint32(data[13:])
		}
		for i := range idx.records {
			if idx.records[i].uid == uid {
				idx.records[i].flags = newFlags
				idx.records[i].modseq = newModSeq
				idx.records[i].keywordBits = kwBits
				break
			}
		}
	case txKeywordUpdate:
		if len(data) < 8 {
			return
		}
		uid := binary.LittleEndian.Uint32(data[0:])
		bits := binary.LittleEndian.Uint32(data[4:])
		if bits != 0 {
			idx.recKeys[uid] = bits
		} else {
			delete(idx.recKeys, uid)
		}
		for i := range idx.records {
			if idx.records[i].uid == uid {
				idx.records[i].keywordBits = bits
				break
			}
		}
	case txExtIntro:
		// keyword names are reloaded from dovecot-keywords on open
	}
}

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

func (idx *IndexFile) writeLogHeader(f *os.File) error {
	buf := make([]byte, 32)
	le := binary.LittleEndian
	buf[0] = logMajor
	buf[1] = logMinor
	le.PutUint32(buf[2:], 32)
	le.PutUint32(buf[6:], idx.indexID)
	le.PutUint32(buf[10:], idx.logFileSeq)
	binary.LittleEndian.PutUint64(buf[18:], idx.modseq)
	le.PutUint32(buf[26:], uint32(time.Now().Unix()))
	_, err := f.Write(buf)
	return err
}

func (idx *IndexFile) appendLogRecord(txType uint32, rec indexRecord) error {
	if idx.logF == nil {
		return nil
	}
	const payloadSize = 17
	const totalSize = 4 + 4 + payloadSize
	buf := make([]byte, totalSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], totalSize)
	le.PutUint32(buf[4:], txType)
	le.PutUint32(buf[8:], rec.uid)
	buf[12] = rec.flags
	le.PutUint64(buf[13:], rec.modseq)
	le.PutUint32(buf[21:], rec.keywordBits)
	_, err := idx.logF.Write(buf)
	return err
}

func (idx *IndexFile) appendKeywordUpdateLog(uid, bits uint32) error {
	if idx.logF == nil {
		return nil
	}
	const totalSize = 4 + 4 + 8
	buf := make([]byte, totalSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], totalSize)
	le.PutUint32(buf[4:], txKeywordUpdate)
	le.PutUint32(buf[8:], uid)
	le.PutUint32(buf[12:], bits)
	_, err := idx.logF.Write(buf)
	return err
}

func (idx *IndexFile) appendExtIntroLog() error {
	if idx.logF == nil {
		return nil
	}
	const name = "keywords"
	nameBytes := []byte(name)
	payloadSize := 4 + len(nameBytes) + 4 + 4 + 4
	totalSize := 4 + 4 + payloadSize
	buf := make([]byte, totalSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], uint32(totalSize))
	le.PutUint32(buf[4:], txExtIntro)
	le.PutUint32(buf[8:], uint32(len(nameBytes)))
	copy(buf[12:], nameBytes)
	off := 12 + len(nameBytes)
	le.PutUint32(buf[off:], kwExt)
	le.PutUint32(buf[off+4:], 0)
	le.PutUint32(buf[off+8:], 0)
	_, err := idx.logF.Write(buf)
	return err
}

func (idx *IndexFile) close() error {
	if idx.logF != nil {
		return idx.logF.Close()
	}
	return nil
}

// ---- keyword helpers -------------------------------------------------------

func (idx *IndexFile) internKeywords(names []string) (uint32, error) {
	if len(names) == 0 {
		return 0, nil
	}
	var bits uint32
	newKeyword := false
	for _, name := range names {
		idx2 := idx.keywordIndex(name)
		if idx2 < 0 {
			if len(idx.keywords) >= maxKeywords {
				return 0, fmt.Errorf("fileindex: keyword limit (%d) reached", maxKeywords)
			}
			idx2 = len(idx.keywords)
			idx.keywords = append(idx.keywords, name)
			if err := idx.saveKeywords(); err != nil {
				return 0, err
			}
			newKeyword = true
		}
		if idx2 < maxKeywords {
			bits |= 1 << uint(idx2)
		}
	}
	if newKeyword && idx.recordSize < baseRecordSize+modseqExt+kwExt {
		idx.recordSize = baseRecordSize + modseqExt + kwExt
		if err := idx.writeHeader(); err != nil {
			return 0, err
		}
		if err := idx.appendExtIntroLog(); err != nil {
			return 0, err
		}
	}
	return bits, nil
}

func (idx *IndexFile) keywordIndex(name string) int {
	for i, kw := range idx.keywords {
		if kw == name {
			return i
		}
	}
	return -1
}

func (idx *IndexFile) bitsToKeywords(bits uint32) []string {
	if bits == 0 {
		return nil
	}
	var out []string
	for i, name := range idx.keywords {
		if i >= maxKeywords {
			break
		}
		if bits&(1<<uint(i)) != 0 {
			out = append(out, name)
		}
	}
	return out
}

func (idx *IndexFile) loadKeywords() {
	f, err := os.Open(idx.path + ".keywords")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			idx.keywords = append(idx.keywords, line)
		}
	}
}

func (idx *IndexFile) saveKeywords() error {
	f, err := os.OpenFile(idx.path+".keywords", os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, kw := range idx.keywords {
		if _, err := fmt.Fprintln(f, kw); err != nil {
			return err
		}
	}
	return nil
}

// ---- filename helpers ------------------------------------------------------

func (idx *IndexFile) loadNames() {
	f, err := os.Open(idx.path + ".names")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		uid64, err := strconv.ParseUint(line[:tab], 10, 32)
		if err != nil {
			continue
		}
		idx.filenames[uint32(uid64)] = line[tab+1:]
	}
}

func (idx *IndexFile) appendNameEntry(uid uint32, filename string) error {
	f, err := os.OpenFile(idx.path+".names", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%d\t%s\n", uid, filename)
	return err
}

// ---- helpers ---------------------------------------------------------------

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
