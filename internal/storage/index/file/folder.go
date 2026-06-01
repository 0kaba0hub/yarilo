package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// OpenFolder opens (or creates) the per-folder index. uidValidity
// is the IMAP UIDVALIDITY value the caller wants stamped on a
// fresh folder; on an existing folder it is ignored (the on-disk
// value is authoritative). Returns a mailbox.Folder snapshot with
// the current state — caller uses Folder.ID for subsequent
// per-folder calls.
//
// On first open of a yarilo-legacy (pre-Phase-2) .index file,
// the legacy decoder is invoked to extract the existing records
// and the index is rewritten atomically as Dovecot-compliant
// before returning. Migration leaves a .legacy backup so an
// operator can roll back manually if needed.
func (u *userIndex) OpenFolder(folder string, uidValidity uint32) (*mailbox.Folder, error) {
	indexDir := u.indexDir(folder)
	indexPath := indexPathFor(indexDir)

	// Dedup: reuse an already-open folderState for the same
	// (user, folder) so consecutive OpenFolder calls in the same
	// session return the same ID.
	u.mu.Lock()
	if u.byDir != nil {
		if id, ok := u.byDir[indexDir]; ok {
			fs := u.open[id]
			u.mu.Unlock()
			snap, err := fs.snapshot(id)
			return snap, err
		}
	}
	u.next++
	id := u.next
	u.mu.Unlock()

	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return nil, fmt.Errorf("fileindex/openfolder: mkdir: %w", err)
	}
	if err := migrateLegacyFilenames(indexDir); err != nil {
		return nil, err
	}

	fs := &folderState{
		folder:    folder,
		indexDir:  indexDir,
		indexPath: indexPath,
		filenames: loadNames(indexDir),
	}
	if err := u.loadOrInit(fs, uidValidity); err != nil {
		return nil, err
	}

	u.mu.Lock()
	u.open[id] = fs
	if u.byDir == nil {
		u.byDir = make(map[string]uint64)
	}
	u.byDir[indexDir] = id
	u.mu.Unlock()
	return fs.snapshot(id)
}

// loadOrInit populates fs.file by reading the existing .index,
// migrating from yarilo-legacy format, or creating a fresh file.
// Caller holds no locks — this runs only once per session-folder.
func (u *userIndex) loadOrInit(fs *folderState, uidValidity uint32) error {
	st, err := os.Stat(fs.indexPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fs.createFresh(uidValidity)
	case err != nil:
		return fmt.Errorf("fileindex/openfolder: stat: %w", err)
	}
	_ = st

	if legacy, isLegacy, err := detectAndDecodeLegacy(fs.indexPath); err != nil {
		return fmt.Errorf("fileindex/openfolder: legacy probe: %w", err)
	} else if isLegacy {
		if err := fs.adoptLegacy(legacy); err != nil {
			return fmt.Errorf("fileindex/openfolder: adopt legacy: %w", err)
		}
		// Migrate atomically: keep the old file as .legacy
		// backup so an operator can roll back the
		// auto-migration if something goes wrong.
		backup := fs.indexPath + ".legacy"
		_ = os.Remove(backup)
		if err := os.Link(fs.indexPath, backup); err != nil {
			debugLog("legacy backup hardlink failed", "err", err)
		}
		if err := fs.flush(true); err != nil {
			return fmt.Errorf("fileindex/openfolder: write migrated: %w", err)
		}
		return ensureLogStub(fs.indexPath, fs.file.Header.IndexID)
	}

	mf, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/openfolder: open: %w", err)
	}
	fs.file = mf
	if err := fs.refreshExtState(); err != nil {
		return err
	}
	return ensureLogStub(fs.indexPath, fs.file.Header.IndexID)
}

// createFresh initialises a brand-new folder state — used both
// for first-ever OpenFolder and as the fallback after a corrupt
// file is moved aside.
func (fs *folderState) createFresh(uidValidity uint32) error {
	indexID := uint32(time.Now().Unix())
	guid := generateGUID()
	exts := defaultExtensions(uidValidity, guid)
	mf, err := mailindex.NewFile(indexID, exts)
	if err != nil {
		return fmt.Errorf("fileindex/createfresh: NewFile: %w", err)
	}
	mf.Header.UIDValidity = uidValidity
	mf.Header.NextUID = 1
	fs.file = mf
	fs.hdr = dboxHdr{MailboxGUID: guid}
	fs.keywords = keywordsHdr{}
	if err := fs.flush(true); err != nil {
		return err
	}
	return ensureLogStub(fs.indexPath, indexID)
}

// refreshExtState re-parses the dbox-hdr and keywords extension
// headers into fs's typed copies. Called after every Open or
// re-read so fs.hdr / fs.keywords reflect the on-disk state.
func (fs *folderState) refreshExtState() error {
	if ext := findExt(fs.file.Extensions, extNameDboxHdr); ext != nil {
		hdr, err := decodeDboxHdr(ext.HdrData)
		if err != nil {
			return fmt.Errorf("fileindex/refresh: dbox-hdr: %w", err)
		}
		fs.hdr = hdr
	}
	if ext := findExt(fs.file.Extensions, extNameKeywords); ext != nil {
		kw, err := decodeKeywordsHdr(ext.HdrData)
		if err != nil {
			return fmt.Errorf("fileindex/refresh: keywords: %w", err)
		}
		fs.keywords = kw
	}
	return nil
}

// snapshot returns a mailbox.Folder describing the current state.
// id is the folderID the userIndex assigned on Open; caller uses
// it for all subsequent per-folder calls.
func (fs *folderState) snapshot(id uint64) (*mailbox.Folder, error) {
	highest, err := fs.highestModSeq()
	if err != nil {
		return nil, err
	}
	unseen := uint32(0)
	if fs.file.Header.MessagesCount > fs.file.Header.SeenMessagesCount {
		unseen = fs.file.Header.MessagesCount - fs.file.Header.SeenMessagesCount
	}
	return &mailbox.Folder{
		ID:            id,
		Name:          fs.folder,
		UIDValidity:   fs.file.Header.UIDValidity,
		NextUID:       fs.file.Header.NextUID,
		Messages:      fs.file.Header.MessagesCount,
		Unseen:        unseen,
		HighestModSeq: highest,
		GUID:          fs.hdr.MailboxGUID,
	}, nil
}

// highestModSeq reads the modseq extension header.
func (fs *folderState) highestModSeq() (uint64, error) {
	ext := findExt(fs.file.Extensions, extNameModSeq)
	if ext == nil {
		return 0, nil
	}
	hdr, err := decodeModseqHdr(ext.HdrData)
	if err != nil {
		return 0, err
	}
	return hdr.HighestModSeq, nil
}

// bumpModSeqHeader increments highest_modseq in the modseq
// extension header and returns the new value. Caller is
// responsible for calling flush afterwards.
func (fs *folderState) bumpModSeqHeader() (uint64, error) {
	ext := findExt(fs.file.Extensions, extNameModSeq)
	if ext == nil {
		return 0, fmt.Errorf("fileindex: modseq extension missing")
	}
	hdr, err := decodeModseqHdr(ext.HdrData)
	if err != nil {
		return 0, err
	}
	hdr.HighestModSeq++
	ext.HdrData = encodeModseqHdr(hdr)
	return hdr.HighestModSeq, nil
}

// advanceModSeqAtLeast bumps highest_modseq to at least target.
// No-op when the on-disk value is already >= target. Used by
// appendLocked / UpdateFlags when the caller pre-allocated a
// modseq via NextModSeq — the header must reflect it but we
// must not bump past it (that would skip values).
func (fs *folderState) advanceModSeqAtLeast(target uint64) error {
	ext := findExt(fs.file.Extensions, extNameModSeq)
	if ext == nil {
		return fmt.Errorf("fileindex: modseq extension missing")
	}
	hdr, err := decodeModseqHdr(ext.HdrData)
	if err != nil {
		return err
	}
	if hdr.HighestModSeq < target {
		hdr.HighestModSeq = target
		ext.HdrData = encodeModseqHdr(hdr)
	}
	return nil
}

// flush rewrites the on-disk .index file from fs.file plus the
// .names sidecar from fs.filenames. The bool wholeNames is
// always true today (we always rewrite .names) — kept as a
// parameter so a future incremental-names optimisation can be
// gated cleanly.
func (fs *folderState) flush(wholeNames bool) error {
	if err := os.MkdirAll(fs.indexDir, 0o700); err != nil {
		return fmt.Errorf("fileindex/flush: mkdir: %w", err)
	}
	if _, err := mailindex.Recreate(fs.file.ToRecreateInput(fs.indexPath)); err != nil {
		return fmt.Errorf("fileindex/flush: recreate: %w", err)
	}
	if wholeNames {
		if err := saveNames(fs.indexDir, fs.filenames); err != nil {
			return err
		}
	}
	return nil
}

// withFolder locates folderID's state, locks it, reloads the
// on-disk snapshot (so a concurrent writer in another process
// can't leave us with stale state), and runs fn. fn sees the
// freshest committed state and is responsible for flushing any
// mutations back via fs.flush. Returns an error when folderID
// isn't open.
//
// reload swallows "file does not exist" so the caller can still
// initialise a fresh folder via createFresh.
func (u *userIndex) withFolder(folderID uint64, fn func(*folderState) error) error {
	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	return u.withFolderLock(fs, func() error {
		if err := fs.reload(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fn(fs)
	})
}

// reload rereads the on-disk .index file into fs. Caller MUST
// hold the folder lock — otherwise another writer could rewrite
// the file mid-read and produce a torn snapshot.
//
// Filenames sidecar is reloaded too. The keyword and dbox-hdr
// extension state derives from the freshly-loaded extensions.
func (fs *folderState) reload() error {
	mf, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/reload: %w", err)
	}
	fs.file = mf
	if err := fs.refreshExtState(); err != nil {
		return err
	}
	fs.filenames = loadNames(fs.indexDir)
	return nil
}

// SaveFolder persists header-level mutations from f back to disk.
// In Phase 2 we re-flush the file in full; legacy semantics that
// SaveFolder only writes the header are preserved by ignoring
// changes to record state (callers do that via AppendMessage /
// UpdateFlags / ExpungeMessage).
func (u *userIndex) SaveFolder(f *mailbox.Folder) error {
	return u.withFolder(f.ID, func(fs *folderState) error {
		fs.file.Header.MessagesCount = f.Messages
		return fs.flush(false)
	})
}

// AppendMessage records m as a new on-disk record. The caller is
// expected to have already assigned m.UID via AllocateUID or via
// an external authority (Dovecot mdbox-style map_uid).
func (u *userIndex) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if err := fs.appendLocked(m); err != nil {
			return err
		}
		return fs.flush(true)
	})
}

// AllocateUID reserves and persists the folder's next UID. The
// caller then passes the UID to UserMailbox.Save and follows up
// with AppendMessage to record the meta. On crash between
// AllocateUID and AppendMessage the UID is burnt — periodic
// rebuild reconciles by scanning the on-disk tree.
//
// Atomic vs concurrent AllocateUID on the same folder: one
// cross-process lock covers the read-modify-write window.
func (u *userIndex) AllocateUID(folderID uint64) (uint32, error) {
	var assigned uint32
	err := u.withFolder(folderID, func(fs *folderState) error {
		uid := fs.file.Header.NextUID
		if uid == 0 {
			uid = 1
		}
		fs.file.Header.NextUID = uid + 1
		assigned = uid
		return fs.flush(false)
	})
	return assigned, err
}

// appendLocked is the in-memory half of AppendMessage. Caller
// must hold the folder lock.
//
// modseq policy: when m.ModSeq is non-zero the caller has
// pre-allocated a value via NextModSeq and we record exactly
// that (bumping the folder high-watermark only if needed so it
// never goes backwards). When m.ModSeq is zero we bump the
// counter ourselves and write the new value into m.
func (fs *folderState) appendLocked(m *mailbox.MessageMeta) error {
	if m.UID == 0 {
		return fmt.Errorf("fileindex/append: UID=0 (use AllocateUID first)")
	}
	var modseq uint64
	if m.ModSeq != 0 {
		modseq = m.ModSeq
		if err := fs.advanceModSeqAtLeast(modseq); err != nil {
			return err
		}
	} else {
		var err error
		modseq, err = fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
	}
	kwBits, kwReg, err := keywordsBitmaskFor(fs.keywords, m.Keywords)
	if err != nil {
		return err
	}
	fs.keywords = kwReg
	if err := fs.persistKeywordRegistry(); err != nil {
		return err
	}
	rec := &mailindex.Record{
		UID:   m.UID,
		Flags: mailindex.MailFlag(imapFlagsToIndex(m.Flags)),
		Ext: map[string][]byte{
			extNameModSeq:   encodeModseqRec(modseq),
			extNameKeywords: encodeKeywordsRec(kwBits),
		},
	}
	fs.file.Records = append(fs.file.Records, rec)
	fs.file.Header.MessagesCount++
	if rec.Flags&mailindex.FlagSeen != 0 {
		fs.file.Header.SeenMessagesCount++
	}
	if rec.Flags&mailindex.FlagDeleted != 0 {
		fs.file.Header.DeletedMessagesCount++
	}
	if m.Filename != "" {
		fs.filenames[m.UID] = m.Filename
	}
	if m.UID >= fs.file.Header.NextUID {
		fs.file.Header.NextUID = m.UID + 1
	}
	m.ModSeq = modseq
	return nil
}

// UpdateFlags replaces the flag set + keyword set for one UID.
// Bumps modseq for that record + the folder header.
func (u *userIndex) UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		modseq, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		kwBits, kwReg, err := keywordsBitmaskFor(fs.keywords, keywords)
		if err != nil {
			return err
		}
		fs.keywords = kwReg
		if err := fs.persistKeywordRegistry(); err != nil {
			return err
		}
		newFlags := mailindex.MailFlag(imapFlagsToIndex(flags))
		for _, rec := range fs.file.Records {
			if rec.UID != uid {
				continue
			}
			oldSeen := rec.Flags&mailindex.FlagSeen != 0
			oldDel := rec.Flags&mailindex.FlagDeleted != 0
			rec.Flags = newFlags
			rec.Ext[extNameModSeq] = encodeModseqRec(modseq)
			rec.Ext[extNameKeywords] = encodeKeywordsRec(kwBits)
			newSeen := newFlags&mailindex.FlagSeen != 0
			newDel := newFlags&mailindex.FlagDeleted != 0
			switch {
			case oldSeen && !newSeen:
				fs.file.Header.SeenMessagesCount--
			case !oldSeen && newSeen:
				fs.file.Header.SeenMessagesCount++
			}
			switch {
			case oldDel && !newDel:
				fs.file.Header.DeletedMessagesCount--
			case !oldDel && newDel:
				fs.file.Header.DeletedMessagesCount++
			}
			break
		}
		return fs.flush(false)
	})
}

// ExpungeMessage removes a record. Writes a TxTypeExpungeGUID
// log entry (with EXPUNGE_PROT) AND removes the record from the
// in-memory state, then Recreates the .index file. Vanished
// later reads those log entries to satisfy QRESYNC.
func (u *userIndex) ExpungeMessage(folderID uint64, uid uint32) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		modseq, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		idx := -1
		for i, rec := range fs.file.Records {
			if rec.UID == uid {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil // already expunged — idempotent
		}
		rec := fs.file.Records[idx]
		if rec.Flags&mailindex.FlagSeen != 0 {
			fs.file.Header.SeenMessagesCount--
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			fs.file.Header.DeletedMessagesCount--
		}
		fs.file.Records = append(fs.file.Records[:idx], fs.file.Records[idx+1:]...)
		fs.file.Header.MessagesCount--
		delete(fs.filenames, uid)

		// Persist expunge in the log so QRESYNC.Vanished can
		// report it. We use TxTypeExpungeGUID; the GUID is the
		// folder's GUID (record-level GUIDs aren't tracked
		// here — Phase 2 limit, refined in mdbox phase).
		if err := appendExpungeRec(fs.indexPath, fs.file.Header.IndexID, uid, modseq, fs.hdr.MailboxGUID); err != nil {
			return fmt.Errorf("fileindex/expunge: log: %w", err)
		}
		return fs.flush(true)
	})
}

// GetMessages returns every record whose UID falls in uids.
// Empty uids means "all records". Output is sorted by UID
// ascending so consumers (QRESYNC, FETCH) get deterministic
// order without re-sorting.
func (u *userIndex) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	var out []*mailbox.MessageMeta
	err := u.withFolder(folderID, func(fs *folderState) error {
		for _, rec := range fs.file.Records {
			if !seqSetContains(uids, rec.UID) {
				continue
			}
			meta := &mailbox.MessageMeta{
				UID:      rec.UID,
				Filename: fs.filenames[rec.UID],
				Flags:    indexFlagsToIMAP(uint8(rec.Flags)),
			}
			if data, ok := rec.Ext[extNameModSeq]; ok {
				meta.ModSeq = decodeModseqRec(data)
			}
			if data, ok := rec.Ext[extNameKeywords]; ok {
				meta.Keywords = keywordsFromBitmask(fs.keywords, decodeKeywordsRec(data))
			}
			out = append(out, meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// NextModSeq bumps the highest_modseq and persists the new value.
// Returns the post-bump value. Used by CONDSTORE writers that
// need to claim a modseq before writing the change.
func (u *userIndex) NextModSeq(folderID uint64) (uint64, error) {
	var out uint64
	err := u.withFolder(folderID, func(fs *folderState) error {
		v, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		out = v
		return fs.flush(false)
	})
	return out, err
}

// Vanished returns every UID expunged from this folder whose
// expunge modseq is strictly greater than sinceModSeq. Drives
// the QRESYNC VANISHED response (RFC 7162). Phase 2 reads the
// minimal expunge records this package writes; full log replay
// (TxTypeExpunge with multi-UID ranges) lands in Phase 2.5.
func (u *userIndex) Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	var out []uint32
	err := u.withFolder(folderID, func(fs *folderState) error {
		uids, err := scanExpungesSince(fs.indexPath, sinceModSeq)
		if err != nil {
			return err
		}
		out = uids
		return nil
	})
	return out, err
}

// Keywords returns the current keyword registry.
func (u *userIndex) Keywords(folderID uint64) ([]string, error) {
	var out []string
	err := u.withFolder(folderID, func(fs *folderState) error {
		out = append([]string(nil), fs.keywords.Names...)
		return nil
	})
	return out, err
}

// ResetFolder replaces every record with the supplied set. Used
// by the admin rebuild flow. Preserves UIDValidity + folder GUID
// + indexID; bumps highest_modseq; sets NextUID past
// max(records.UID).
func (u *userIndex) ResetFolder(folderID uint64, records []*mailbox.MessageMeta) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		modseq, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		fs.file.Records = fs.file.Records[:0]
		fs.filenames = make(map[uint32]string)
		fs.file.Header.MessagesCount = 0
		fs.file.Header.SeenMessagesCount = 0
		fs.file.Header.DeletedMessagesCount = 0

		var maxUID uint32
		for _, m := range records {
			if m == nil || m.UID == 0 {
				continue
			}
			kwBits, kwReg, err := keywordsBitmaskFor(fs.keywords, m.Keywords)
			if err != nil {
				return err
			}
			fs.keywords = kwReg
			rec := &mailindex.Record{
				UID:   m.UID,
				Flags: mailindex.MailFlag(imapFlagsToIndex(m.Flags)),
				Ext: map[string][]byte{
					extNameModSeq:   encodeModseqRec(modseq),
					extNameKeywords: encodeKeywordsRec(kwBits),
				},
			}
			fs.file.Records = append(fs.file.Records, rec)
			fs.file.Header.MessagesCount++
			if rec.Flags&mailindex.FlagSeen != 0 {
				fs.file.Header.SeenMessagesCount++
			}
			if rec.Flags&mailindex.FlagDeleted != 0 {
				fs.file.Header.DeletedMessagesCount++
			}
			if m.Filename != "" {
				fs.filenames[m.UID] = m.Filename
			}
			if m.UID > maxUID {
				maxUID = m.UID
			}
		}
		if maxUID >= fs.file.Header.NextUID {
			fs.file.Header.NextUID = maxUID + 1
		}
		if err := fs.persistKeywordRegistry(); err != nil {
			return err
		}
		return fs.flush(true)
	})
}

// OptimizeIndex compacts pending log records into the base file.
// In Phase 2 the log file holds only expunge records that
// Vanished reads; OptimizeIndex truncates them after a successful
// .index Recreate so future Vanished calls only see post-compact
// state (matching Dovecot's compaction semantics).
//
// Caller's expectation: post-OptimizeIndex, Vanished(sinceModSeq)
// returns empty for every sinceModSeq < currentHighest, because
// every prior expunge has been "absorbed" into the base index
// (the records simply don't exist anymore).
func (u *userIndex) OptimizeIndex(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if err := fs.flush(true); err != nil {
			return err
		}
		return truncateLog(fs.indexPath, fs.file.Header.IndexID)
	})
}

// persistKeywordRegistry encodes fs.keywords back into the
// keywords extension's HdrData. Called after every mutation that
// might have added new keyword names.
func (fs *folderState) persistKeywordRegistry() error {
	ext := findExt(fs.file.Extensions, extNameKeywords)
	if ext == nil {
		return fmt.Errorf("fileindex: keywords extension missing")
	}
	ext.HdrData = encodeKeywordsHdr(fs.keywords)
	ext.HdrSize = uint32(len(ext.HdrData))
	// Recompute header size since the extension header may have
	// grown / shrunk.
	exts := fs.file.Extensions
	extBytes, err := mailindex.EncodeExtHeaders(exts)
	if err != nil {
		return err
	}
	fs.file.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	return nil
}

// adoptLegacy populates fs from a legacy-decoded snapshot. The
// caller calls flush after to materialise the Dovecot format.
func (fs *folderState) adoptLegacy(snap legacySnapshot) error {
	exts := defaultExtensions(snap.UIDValidity, snap.MailboxGUID)
	mf, err := mailindex.NewFile(snap.IndexID, exts)
	if err != nil {
		return err
	}
	mf.Header.UIDValidity = snap.UIDValidity
	mf.Header.NextUID = snap.NextUID
	mf.Header.MessagesCount = uint32(len(snap.Records))
	for _, rec := range snap.Records {
		if rec.Flags&mailindex.FlagSeen != 0 {
			mf.Header.SeenMessagesCount++
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			mf.Header.DeletedMessagesCount++
		}
		mf.Records = append(mf.Records, rec)
	}
	if modseqExt := findExt(mf.Extensions, extNameModSeq); modseqExt != nil {
		modseqExt.HdrData = encodeModseqHdr(modseqHdr{HighestModSeq: snap.HighestModSeq})
	}
	fs.file = mf
	fs.hdr = dboxHdr{MailboxGUID: snap.MailboxGUID}
	fs.keywords = snap.Keywords
	if err := fs.persistKeywordRegistry(); err != nil {
		return err
	}
	fs.filenames = snap.Filenames
	return nil
}

// ---- per-record encoders -----------------------------------

func encodeModseqRec(v uint64) []byte {
	out := make([]byte, modseqRecSize)
	binary.LittleEndian.PutUint64(out, v)
	return out
}

func decodeModseqRec(b []byte) uint64 {
	if len(b) < modseqRecSize {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func encodeKeywordsRec(bits uint32) []byte {
	out := make([]byte, keywordsRecSize)
	binary.LittleEndian.PutUint32(out, bits)
	return out
}

func decodeKeywordsRec(b []byte) uint32 {
	if len(b) < keywordsRecSize {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// ---- log file expunge tracking -----------------------------

// appendExpungeRec appends one TxTypeExpungeGUID record to the
// .log file. The record carries (uid, guid, modseq) — Vanished
// uses the modseq to filter and the uid to report.
//
// Phase 2 uses a minimal log writer here rather than expanding
// mailindex's API. Phase 2.5 will replace these helpers with a
// proper mailindex.LogWriter.
func appendExpungeRec(indexPath string, indexID, uid uint32, modseq uint64, guid [16]byte) error {
	logPath := indexPath + ".log"
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/log: open: %w", err)
	}
	defer f.Close()
	// Header if file is fresh.
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		hdr := mailindex.NewLogHeader(indexID, 1, uint32(time.Now().Unix()))
		if err := hdr.Encode(f); err != nil {
			return fmt.Errorf("fileindex/log: write header: %w", err)
		}
	}
	// Payload: TxExpungeGUID (20 bytes) + 8 bytes of trailing
	// modseq encoded inline. Real Dovecot doesn't carry modseq
	// in the EXPUNGE_GUID payload — they derive it from a
	// MODSEQ_UPDATE record that precedes the expunge. For
	// Phase 2 we keep it self-contained: a 28-byte payload of
	// {uid:4, guid:16, modseq:8} so Vanished doesn't need to
	// chase MODSEQ_UPDATE records. Phase 2.5 will switch to
	// canonical (MODSEQ_UPDATE + EXPUNGE_GUID) pairs.
	payload := make([]byte, 28)
	le := binary.LittleEndian
	le.PutUint32(payload[0:], uid)
	copy(payload[4:20], guid[:])
	le.PutUint64(payload[20:], modseq)

	hdrBuf := make([]byte, 8)
	hdr := mailindex.TxHeader{
		Size: 8 + uint32(len(payload)),
		Type: mailindex.TxTypeFlags(mailindex.TxTypeExpungeGUID) | mailindex.TxExpungeProt,
	}
	if err := mailindex.EncodeTxHeader(hdrBuf, hdr); err != nil {
		return fmt.Errorf("fileindex/log: encode tx hdr: %w", err)
	}
	if _, err := f.Write(hdrBuf); err != nil {
		return fmt.Errorf("fileindex/log: write tx hdr: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("fileindex/log: write payload: %w", err)
	}
	return nil
}

// scanExpungesSince reads every TxTypeExpungeGUID record in
// the .log file and returns the UIDs whose embedded modseq is
// strictly greater than sinceModSeq. Returns an empty slice
// when the log doesn't exist or contains no matching records.
func scanExpungesSince(indexPath string, sinceModSeq uint64) ([]uint32, error) {
	logPath := indexPath + ".log"
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fileindex/log scan: open: %w", err)
	}
	defer f.Close()
	if _, err := mailindex.DecodeLogHeader(f); err != nil {
		// Treat header errors as "empty log" — a missing or
		// stub header has no records to skip over.
		return nil, nil //nolint:nilerr
	}
	var out []uint32
	hdrBuf := make([]byte, 8)
	for {
		_, err := io.ReadFull(f, hdrBuf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("fileindex/log scan: read hdr: %w", err)
		}
		txHdr, err := mailindex.DecodeTxHeader(hdrBuf)
		if err != nil {
			break // torn write — stop here; subsequent records are unrecoverable
		}
		payloadLen := int(txHdr.Size) - 8
		if payloadLen < 0 {
			break
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(f, payload); err != nil {
			break
		}
		if txHdr.Type.Kind() != mailindex.TxTypeExpungeGUID|mailindex.TxType(mailindex.TxExpungeProt) {
			continue
		}
		if len(payload) < 28 {
			continue
		}
		uid := binary.LittleEndian.Uint32(payload[0:])
		modseq := binary.LittleEndian.Uint64(payload[20:])
		if modseq > sinceModSeq {
			out = append(out, uid)
		}
	}
	return out, nil
}

// truncateLog drops every expunge record. Called by
// OptimizeIndex after a successful base-file rewrite — the
// records have been "absorbed" into the snapshot.
func truncateLog(indexPath string, indexID uint32) error {
	logPath := indexPath + ".log"
	tmp := logPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/log truncate: open: %w", err)
	}
	hdr := mailindex.NewLogHeader(indexID, 1, uint32(time.Now().Unix()))
	if err := hdr.Encode(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log truncate: header: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log truncate: close: %w", err)
	}
	if err := os.Rename(tmp, logPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fileindex/log truncate: rename: %w", err)
	}
	return nil
}
