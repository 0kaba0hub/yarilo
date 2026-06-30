package file

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

var errLogIndexIDMismatch = errors.New("fileindex: log IndexID does not match base index")

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
	// session return the same ID. Reload the on-disk state first so
	// the returned snapshot reflects writes from other sessions/pods.
	u.mu.Lock()
	if u.byDir != nil {
		if id, ok := u.byDir[indexDir]; ok {
			u.mu.Unlock()
			var snap *mailbox.Folder
			err := u.withFolderRO(id, func(fs *folderState) error {
				var sErr error
				snap, sErr = fs.snapshot(id)
				return sErr
			})
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

	names, sizes := loadNames(indexDir)
	fs := &folderState{
		folder:      folder,
		indexDir:    indexDir,
		indexPath:   indexPath,
		volatileDir: u.folderVolatileDir(folder),
		filenames:   names,
		sizes:       sizes,
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
	if st, stErr := os.Stat(fs.indexPath); stErr == nil {
		fs.baseMod = st.ModTime()
	}
	if logSt, logErr := os.Stat(fs.indexPath + ".log"); logErr == nil {
		if applyErr := fs.applyLog(0); applyErr != nil && !errors.Is(applyErr, errLogIndexIDMismatch) {
			return fmt.Errorf("fileindex/openfolder: applylog: %w", applyErr)
		}
		fs.logSize = logSt.Size()
	}
	if fs.file.Header.UIDValidity == 0 {
		fs.file.Header.UIDValidity = uint32(time.Now().Unix())
		if err := fs.flush(true); err != nil {
			return fmt.Errorf("fileindex/openfolder: fix uidvalidity: %w", err)
		}
	}
	if err := fs.refreshExtState(); err != nil {
		return err
	}
	return ensureLogStub(fs.indexPath, fs.file.Header.IndexID)
}

// createFresh initialises a brand-new folder state — used both
// for first-ever OpenFolder and as the fallback after a corrupt
// file is moved aside.
func (fs *folderState) createFresh(uidValidity uint32) error {
	if uidValidity == 0 {
		uidValidity = uint32(time.Now().Unix())
	}
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
	ri := fs.file.ToRecreateInput(fs.indexPath)
	if fs.volatileDir != "" {
		if err := os.MkdirAll(fs.volatileDir, 0o700); err != nil {
			return fmt.Errorf("fileindex/flush: mkdir volatile: %w", err)
		}
		ri.TmpDir = fs.volatileDir
	}
	if _, err := mailindex.Recreate(ri); err != nil {
		return fmt.Errorf("fileindex/flush: recreate: %w", err)
	}
	if wholeNames {
		if err := saveNames(fs.indexDir, fs.filenames, fs.sizes); err != nil {
			return err
		}
	}
	// Track base mtime so reload() fast-path fires after this flush.
	if st, _ := os.Stat(fs.indexPath); st != nil {
		fs.baseMod = st.ModTime()
	}
	fs.lastFlush = time.Now()
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

// reload rereads the on-disk state into fs. Caller MUST hold the
// folder lock. It uses a two-stage fast path:
//
//  1. Stat the base .index and .index.log. If neither has changed
//     since the last full reload, return immediately — this is the
//     common case within a single pod where fs.file is already
//     current.
//
//  2. If the base file is unchanged but the log has grown, apply
//     only the new log entries via applyLog so fs.file reflects
//     any cross-pod writes without a full base re-read.
//
//  3. If the base file changed (after OptimizeIndex), do a full
//     re-read of base + remaining log.
func (fs *folderState) reload() error {
	baseStat, baseErr := os.Stat(fs.indexPath)
	logStat, _ := os.Stat(fs.indexPath + ".log")

	var newLogSize int64
	if logStat != nil {
		newLogSize = logStat.Size()
	}

	var newBaseMod time.Time
	if baseStat != nil {
		newBaseMod = baseStat.ModTime()
	}

	// Fast path: nothing on disk changed.
	if newBaseMod == fs.baseMod && newLogSize == fs.logSize {
		return nil
	}

	// Base file changed (or first open) → full reload.
	if newBaseMod != fs.baseMod || fs.file == nil {
		if baseErr != nil {
			return fmt.Errorf("fileindex/reload: %w", baseErr)
		}
		mf, err := mailindex.Open(fs.indexPath)
		if err != nil {
			return fmt.Errorf("fileindex/reload: %w", err)
		}
		fs.file = mf
		if err := fs.refreshExtState(); err != nil {
			return err
		}
		fs.filenames, fs.sizes = loadNames(fs.indexDir)
		fs.baseMod = newBaseMod
		fs.logSize = 0 // will be updated by applyLog below
	}

	// Apply any log entries added since logSize (by another pod or
	// by our own appends that a concurrent reload must see).
	if newLogSize > fs.logSize {
		if err := fs.applyLog(fs.logSize); errors.Is(err, errLogIndexIDMismatch) {
			// Stale log from a previous mailbox at this path — flush the
			// current base to disk and reset the log so future reads start clean.
			slog.Warn("fileindex: discarding log with mismatched IndexID, re-flushing base",
				"folder", fs.folder)
			if flushErr := fs.flush(false); flushErr != nil {
				return fmt.Errorf("fileindex/reload: flush after indexid mismatch: %w", flushErr)
			}
			if truncErr := truncateLog(fs.indexPath, fs.file.Header.IndexID); truncErr != nil {
				return fmt.Errorf("fileindex/reload: truncate after indexid mismatch: %w", truncErr)
			}
			fs.logSize = 0
		} else if err != nil {
			return err
		} else {
			fs.logSize = newLogSize
		}
	}
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
	var folder string
	if err := u.withFolder(folderID, func(fs *folderState) error {
		folder = fs.folder
		if err := fs.appendLocked(m); err != nil {
			return err
		}
		if err := fs.flushAppend(fs.file.Records[len(fs.file.Records)-1]); err != nil {
			return err
		}
		u.compactLogIfNeeded(fs)
		return nil
	}); err != nil {
		return err
	}
	u.quotaAdd(folder, int64(m.Size), 1)
	return nil
}

// quotaAdd adjusts the user's quota counter for the given folder.
// Skips the update when the folder is configured as "ignore".
// Errors are logged but never returned — drift is recoverable via admin recalc.
func (u *userIndex) quotaAdd(folder string, bytes, messages int64) {
	if u.counter == nil {
		return
	}
	if _, ignore := u.limits.EffectiveLimits(folder); ignore {
		return
	}
	if err := u.counter.Add(context.Background(), bytes, messages); err != nil {
		slog.Warn("fileindex: quota counter update failed",
			"user", u.username, "folder", folder, "bytes", bytes, "messages", messages, "err", err)
	}
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
		return fs.appendMutLog(encU32Update(28, fs.file.Header.NextUID))
	})
	return assigned, err
}

func (u *userIndex) AllocateUIDWithModSeq(folderID uint64) (uint32, uint64, error) {
	var uid uint32
	var modseq uint64
	err := u.withFolder(folderID, func(fs *folderState) error {
		next := fs.file.Header.NextUID
		if next == 0 {
			next = 1
		}
		fs.file.Header.NextUID = next + 1
		uid = next
		var err error
		modseq, err = fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		return fs.appendMutLog(encU32Update(28, fs.file.Header.NextUID))
	})
	return uid, modseq, err
}

func (u *userIndex) AllocateAndAppend(folderID uint64, m *mailbox.MessageMeta) error {
	var folder string
	if err := u.withFolder(folderID, func(fs *folderState) error {
		folder = fs.folder
		next := fs.file.Header.NextUID
		if next == 0 {
			next = 1
		}
		fs.file.Header.NextUID = next + 1
		m.UID = next
		if err := fs.appendLocked(m); err != nil {
			return err
		}
		if err := fs.flushAppend(fs.file.Records[len(fs.file.Records)-1]); err != nil {
			return err
		}
		u.compactLogIfNeeded(fs)
		return nil
	}); err != nil {
		return err
	}
	u.quotaAdd(folder, int64(m.Size), 1)
	return nil
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
	prevKwCount := len(fs.keywords.Names)
	kwBits, kwReg, err := keywordsBitmaskFor(fs.keywords, m.Keywords)
	if err != nil {
		return err
	}
	fs.keywords = kwReg
	if err := fs.persistKeywordRegistry(); err != nil {
		return err
	}
	// Keyword registry grew — persist extension headers to the base file now
	// so a cross-pod reader or pod restart can decode keyword bitmasks.
	// This is a rare write (only on first use of each keyword name).
	if len(fs.keywords.Names) > prevKwCount {
		if err := fs.flush(false); err != nil {
			return err
		}
	}
	flags := mailindex.MailFlag(imapFlagsToIndex(m.Flags))
	if m.AltTier {
		flags |= mailindex.FlagBackend
	}
	rec := &mailindex.Record{
		UID:   m.UID,
		Flags: flags,
		Ext: map[string][]byte{
			extNameModSeq:       encodeModseqRec(modseq),
			extNameKeywords:     encodeKeywordsRec(kwBits),
			extNameInternalDate: encodeIdateRec(m.InternalDate),
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
	fs.sizes[m.UID] = m.Size
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
			// Preserve the backend-private AltTier bit — IMAP STORE must
			// not clear a tier marker it knows nothing about.
			newFlags |= rec.Flags & mailindex.FlagBackend
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
		return fs.appendMutLog(
			encLogRec(mailindex.TxTypeModseqUpdate, 0, mailindex.EncodeTxModseqUpdatePayload([]mailindex.TxModseqUpdate{{
				UID: uid, ModSeqLow32: uint32(modseq), ModSeqHigh32: uint32(modseq >> 32),
			}})),
			encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload([]mailindex.TxFlagUpdate{{
				UID1: uid, UID2: uid, AddFlags: newFlags, RemoveFlags: ^newFlags,
			}})),
			encU32Update(40, fs.file.Header.SeenMessagesCount),
			encU32Update(44, fs.file.Header.DeletedMessagesCount),
		)
	})
}

// UpdateFlagsMulti replaces flags+keywords for a batch of UIDs in a single
// lock/reload/flush cycle. Each UID gets an individual modseq bump so clients
// can use CONDSTORE to pinpoint which messages changed. Returns the new modseq
// per UID; UIDs not found in the index are silently skipped.
func (u *userIndex) UpdateFlagsMulti(folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]uint64, error) {
	result := make(map[uint32]uint64, len(updates))
	err := u.withFolder(folderID, func(fs *folderState) error {
		// Collect all unique keyword sets across the batch to register them first.
		allKWs := make([]string, 0)
		seen := make(map[string]struct{})
		for _, upd := range updates {
			for _, kw := range upd.Keywords {
				if _, ok := seen[kw]; !ok {
					seen[kw] = struct{}{}
					allKWs = append(allKWs, kw)
				}
			}
		}
		if len(allKWs) > 0 {
			_, kwReg, err := keywordsBitmaskFor(fs.keywords, allKWs)
			if err != nil {
				return err
			}
			fs.keywords = kwReg
			if err := fs.persistKeywordRegistry(); err != nil {
				return err
			}
		}

		var modseqUpdates []mailindex.TxModseqUpdate
		var flagUpdates []mailindex.TxFlagUpdate
		for _, rec := range fs.file.Records {
			upd, ok := updates[rec.UID]
			if !ok {
				continue
			}
			modseq, err := fs.bumpModSeqHeader()
			if err != nil {
				return err
			}
			kwBits, _, err := keywordsBitmaskFor(fs.keywords, upd.Keywords)
			if err != nil {
				return err
			}
			newFlags := mailindex.MailFlag(imapFlagsToIndex(upd.Flags))
			oldSeen := rec.Flags&mailindex.FlagSeen != 0
			oldDel := rec.Flags&mailindex.FlagDeleted != 0
			newFlags |= rec.Flags & mailindex.FlagBackend
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
			result[rec.UID] = modseq
			modseqUpdates = append(modseqUpdates, mailindex.TxModseqUpdate{
				UID: rec.UID, ModSeqLow32: uint32(modseq), ModSeqHigh32: uint32(modseq >> 32),
			})
			flagUpdates = append(flagUpdates, mailindex.TxFlagUpdate{
				UID1: rec.UID, UID2: rec.UID, AddFlags: newFlags, RemoveFlags: ^newFlags,
			})
		}
		if len(modseqUpdates) == 0 {
			return nil
		}
		return fs.appendMutLog(
			encLogRec(mailindex.TxTypeModseqUpdate, 0, mailindex.EncodeTxModseqUpdatePayload(modseqUpdates)),
			encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload(flagUpdates)),
			encU32Update(40, fs.file.Header.SeenMessagesCount),
			encU32Update(44, fs.file.Header.DeletedMessagesCount),
		)
	})
	return result, err
}

// ExpungeMessage removes a record. Writes a TxTypeExpungeGUID
// log entry (with EXPUNGE_PROT) AND removes the record from the
// in-memory state, then Recreates the .index file. Vanished
// later reads those log entries to satisfy QRESYNC.
func (u *userIndex) ExpungeMessage(folderID uint64, uid uint32) error {
	var expungedSize uint32
	var expungedFolder string
	if err := u.withFolder(folderID, func(fs *folderState) error {
		expungedFolder = fs.folder
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
		expungedSize = fs.sizes[uid]
		if rec.Flags&mailindex.FlagSeen != 0 {
			fs.file.Header.SeenMessagesCount--
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			fs.file.Header.DeletedMessagesCount--
		}
		fs.file.Records = append(fs.file.Records[:idx], fs.file.Records[idx+1:]...)
		fs.file.Header.MessagesCount--
		delete(fs.filenames, uid)
		delete(fs.sizes, uid)

		// 28-byte payload: uid(4)+guid(16)+modseq(8). Compatible with
		// scanExpungesSince which reads the same layout.
		expPayload := make([]byte, 28)
		le := binary.LittleEndian
		le.PutUint32(expPayload[0:], uid)
		copy(expPayload[4:20], fs.hdr.MailboxGUID[:])
		le.PutUint64(expPayload[20:], modseq)
		return fs.appendMutLog(
			encLogRec(mailindex.TxTypeExpungeGUID, mailindex.TxExpungeProt, expPayload),
			encU32Update(32, fs.file.Header.MessagesCount),
			encU32Update(40, fs.file.Header.SeenMessagesCount),
			encU32Update(44, fs.file.Header.DeletedMessagesCount),
		)
	}); err != nil {
		return err
	}
	u.quotaAdd(expungedFolder, -int64(expungedSize), -1)
	return nil
}

// GetMessages returns every record whose UID falls in uids.
// Empty uids means "all records". Output is sorted by UID
// ascending so consumers (QRESYNC, FETCH) get deterministic
// order without re-sorting.
func (u *userIndex) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	var out []*mailbox.MessageMeta
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		for _, rec := range fs.file.Records {
			if !seqSetContains(uids, rec.UID) {
				continue
			}
			meta := &mailbox.MessageMeta{
				UID:      rec.UID,
				Filename: fs.filenames[rec.UID],
				Flags:    indexFlagsToIMAP(uint8(rec.Flags)),
				Size:     fs.sizes[rec.UID],
				AltTier:  rec.Flags&mailindex.FlagBackend != 0,
			}
			if data, ok := rec.Ext[extNameModSeq]; ok {
				meta.ModSeq = decodeModseqRec(data)
			}
			if data, ok := rec.Ext[extNameKeywords]; ok {
				meta.Keywords = keywordsFromBitmask(fs.keywords, decodeKeywordsRec(data))
			}
			if data, ok := rec.Ext[extNameInternalDate]; ok {
				meta.InternalDate = decodeIdateRec(data)
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
		// No flush needed — the modseq will be written by the subsequent
		// AppendMessage / UpdateFlags TxModseqUpdate log record.
		return nil
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
	err := u.withFolderRO(folderID, func(fs *folderState) error {
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
	err := u.withFolderRO(folderID, func(fs *folderState) error {
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
		fs.sizes = make(map[uint32]uint32)
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
			fs.sizes[m.UID] = m.Size
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
		if err := fs.flush(true); err != nil {
			return err
		}
		// Truncate the log so stale TxAppend records don't resurface
		// when another process replays the log after ResetFolder.
		if err := truncateLog(fs.indexPath, fs.file.Header.IndexID); err != nil {
			return err
		}
		fs.logSize = 0
		return nil
	})
}

// SetAltTier sets or clears FlagBackend on every record whose Filename
// is in the filenames set. Called after mdbox AltMove physically
// relocates m.<N> files so subsequent Fetch calls skip the primary
// open() syscall for cold-tier messages.
func (u *userIndex) SetAltTier(folderID uint64, filenames []string, altTier bool) error {
	if len(filenames) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(filenames))
	for _, f := range filenames {
		set[f] = struct{}{}
	}
	return u.withFolder(folderID, func(fs *folderState) error {
		changed := false
		for _, rec := range fs.file.Records {
			fn := fs.filenames[rec.UID]
			if _, ok := set[fn]; !ok {
				continue
			}
			before := rec.Flags
			if altTier {
				rec.Flags |= mailindex.FlagBackend
			} else {
				rec.Flags &^= mailindex.FlagBackend
			}
			if rec.Flags != before {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return fs.flush(false)
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
		if err := truncateLog(fs.indexPath, fs.file.Header.IndexID); err != nil {
			return err
		}
		// Log was reset to an empty header; next reload() will fast-path base
		// (baseMod already updated by flush) and apply zero log records.
		fs.logSize = 0
		return nil
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
	fs.sizes = make(map[uint32]uint32)
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

// ---- mutation log (Phase 2.5) --------------------------------

// encLogRec encodes a complete tx record: 8-byte TxHeader + payload.
func encLogRec(txType mailindex.TxType, extraType mailindex.TxTypeFlags, payload []byte) []byte {
	hdrBuf := make([]byte, 8)
	_ = mailindex.EncodeTxHeader(hdrBuf, mailindex.TxHeader{
		Size: uint32(8 + len(payload)),
		Type: mailindex.TxTypeFlags(txType) | extraType,
	})
	out := make([]byte, 8+len(payload))
	copy(out, hdrBuf)
	copy(out[8:], payload)
	return out
}

// encU32Update encodes a TxTypeHeaderUpdate record patching a single uint32
// field at the given byte offset of the base index header.
func encU32Update(offset uint16, v uint32) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, v)
	return encLogRec(mailindex.TxTypeHeaderUpdate, 0,
		mailindex.EncodeTxHeaderUpdatePayload(mailindex.TxHeaderUpdate{Offset: offset, Data: data}))
}

// appendMutLog writes pre-encoded tx records to the .index.log file and
// updates fs.logSize. Caller must hold fs.mu (guaranteed by withFolder).
func (fs *folderState) appendMutLog(records ...[]byte) error {
	logPath := fs.indexPath + ".log"
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/mutlog: open: %w", err)
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		hdr := mailindex.NewLogHeader(fs.file.Header.IndexID, 1, uint32(time.Now().Unix()))
		if err := hdr.Encode(f); err != nil {
			return fmt.Errorf("fileindex/mutlog: write header: %w", err)
		}
	}
	for _, rec := range records {
		if _, err := f.Write(rec); err != nil {
			return fmt.Errorf("fileindex/mutlog: write: %w", err)
		}
	}
	if st, _ := f.Stat(); st != nil {
		fs.logSize = st.Size()
	}
	return nil
}

// applyLog reads tx records from .index.log starting at fromOffset and
// applies them to fs.file. Called by reload when the log has grown since
// the last full base read (cross-pod writes). Caller must hold fs.mu.
//
// Keywords extension data is NOT updated from log records — that is a
// known Phase 2.5 limitation; cross-pod keyword visibility requires
// OptimizeIndex to compact the log into the base file.
func (fs *folderState) applyLog(fromOffset int64) error {
	logPath := fs.indexPath + ".log"
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fileindex/applylog: open: %w", err)
	}
	defer f.Close()

	lh, hdrErr := mailindex.DecodeLogHeader(f)
	if hdrErr != nil {
		return nil // empty or unreadable log
	}
	if lh.IndexID != fs.file.Header.IndexID {
		// Log belongs to a different (deleted/recreated) mailbox at this path.
		// Discard it; caller will flush a fresh base + empty log.
		return errLogIndexIDMismatch
	}
	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return fmt.Errorf("fileindex/applylog: seek: %w", err)
		}
	}

	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/applylog: record layout: %w", err)
	}

	var maxModseq uint64
	le := binary.LittleEndian
	hdrBuf := make([]byte, 8)
	appendedMsgs := false

	for {
		if _, err := io.ReadFull(f, hdrBuf); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		} else if err != nil {
			return fmt.Errorf("fileindex/applylog: read hdr: %w", err)
		}
		txHdr, err := mailindex.DecodeTxHeader(hdrBuf)
		if err != nil {
			break // torn write — stop here
		}
		payloadLen := int(txHdr.Size) - 8
		if payloadLen < 0 {
			break
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(f, payload); err != nil {
			break
		}

		kind := txHdr.Type.Kind()

		switch {
		case kind == mailindex.TxTypeModseqUpdate:
			for i := 0; i+12 <= len(payload); i += 12 {
				uid := le.Uint32(payload[i:])
				modseq := uint64(le.Uint32(payload[i+4:])) | uint64(le.Uint32(payload[i+8:]))<<32
				if modseq > maxModseq {
					maxModseq = modseq
				}
				for _, rec := range fs.file.Records {
					if rec.UID == uid {
						if rec.Ext == nil {
							rec.Ext = make(map[string][]byte)
						}
						rec.Ext[extNameModSeq] = encodeModseqRec(modseq)
						break
					}
				}
			}

		case kind == mailindex.TxTypeFlagUpdate:
			for i := 0; i+12 <= len(payload); i += 12 {
				uid1 := le.Uint32(payload[i:])
				uid2 := le.Uint32(payload[i+4:])
				addFlags := mailindex.MailFlag(payload[i+8])
				removeFlags := mailindex.MailFlag(payload[i+9])
				for _, rec := range fs.file.Records {
					if rec.UID >= uid1 && rec.UID <= uid2 {
						rec.Flags = (rec.Flags | addFlags) &^ removeFlags
					}
				}
			}

		case kind == mailindex.TxTypeExpungeGUID|mailindex.TxType(mailindex.TxExpungeProt):
			// Accept 28-byte legacy format (uid+guid+modseq) and 20-byte canonical.
			stride := 20
			if len(payload) > 0 && len(payload)%28 == 0 && len(payload)%20 != 0 {
				stride = 28
			}
			for i := 0; i+stride <= len(payload); i += stride {
				uid := le.Uint32(payload[i:])
				for j, rec := range fs.file.Records {
					if rec.UID != uid {
						continue
					}
					if rec.Flags&mailindex.FlagSeen != 0 {
						fs.file.Header.SeenMessagesCount--
					}
					if rec.Flags&mailindex.FlagDeleted != 0 {
						fs.file.Header.DeletedMessagesCount--
					}
					fs.file.Records = append(fs.file.Records[:j], fs.file.Records[j+1:]...)
					fs.file.Header.MessagesCount--
					break
				}
			}

		case kind == mailindex.TxTypeHeaderUpdate:
			for i := 0; i+4 <= len(payload); {
				offset := le.Uint16(payload[i:])
				size := le.Uint16(payload[i+2:])
				i += 4
				if i+int(size) > len(payload) {
					break
				}
				data := payload[i : i+int(size)]
				i += int(size)
				pad := (4 - ((4 + int(size)) % 4)) % 4
				i += pad
				if size == 4 {
					v := le.Uint32(data)
					switch offset {
					case 28:
						fs.file.Header.NextUID = v
					case 32:
						fs.file.Header.MessagesCount = v
					case 40:
						fs.file.Header.SeenMessagesCount = v
					case 44:
						fs.file.Header.DeletedMessagesCount = v
					}
				}
			}

		case kind == mailindex.TxTypeAppend:
			stride := int(layout.RecordSize)
			if stride == 0 {
				break
			}
			// Build a UID set from existing records for O(1) dedup.
			existing := make(map[uint32]struct{}, len(fs.file.Records))
			for _, r := range fs.file.Records {
				existing[r.UID] = struct{}{}
			}
			for i := 0; i+stride <= len(payload); i += stride {
				rec, recErr := mailindex.DecodeRecord(payload[i:i+stride], layout)
				if recErr != nil {
					break
				}
				if _, dup := existing[rec.UID]; dup {
					continue // already present from base file or earlier log replay
				}
				rp := rec
				fs.file.Records = append(fs.file.Records, &rp)
				existing[rp.UID] = struct{}{}
				fs.file.Header.MessagesCount++
				if rp.Flags&mailindex.FlagSeen != 0 {
					fs.file.Header.SeenMessagesCount++
				}
				if rp.Flags&mailindex.FlagDeleted != 0 {
					fs.file.Header.DeletedMessagesCount++
				}
				appendedMsgs = true
			}
		}
	}

	if appendedMsgs {
		fs.filenames, fs.sizes = loadNames(fs.indexDir)
	}

	if maxModseq > 0 {
		if ext := findExt(fs.file.Extensions, extNameModSeq); ext != nil {
			if hdr, hdrErr := decodeModseqHdr(ext.HdrData); hdrErr == nil && maxModseq > hdr.HighestModSeq {
				hdr.HighestModSeq = maxModseq
				ext.HdrData = encodeModseqHdr(hdr)
			}
		}
	}
	return nil
}

// flushAppend persists a newly appended record to the log and updates the
// filenames sidecar. rec must be the record that was just added to
// fs.file.Records (i.e. the last element). Caller must hold fs.mu.
func (fs *folderState) flushAppend(rec *mailindex.Record) error {
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/append: layout: %w", err)
	}
	appendPayload, err := mailindex.EncodeTxAppendPayload(layout, []*mailindex.Record{rec})
	if err != nil {
		return fmt.Errorf("fileindex/append: encode: %w", err)
	}
	if err := saveNames(fs.indexDir, fs.filenames, fs.sizes); err != nil {
		return fmt.Errorf("fileindex/append: names: %w", err)
	}
	return fs.appendMutLog(
		encLogRec(mailindex.TxTypeAppend, 0, appendPayload),
		encU32Update(28, fs.file.Header.NextUID),
		encU32Update(32, fs.file.Header.MessagesCount),
		encU32Update(40, fs.file.Header.SeenMessagesCount),
		encU32Update(44, fs.file.Header.DeletedMessagesCount),
	)
}

// ---- log file expunge tracking (legacy, pre-Phase-2.5) --------------------

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
