package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var errLogIndexIDMismatch = errors.New("fileindex: log IndexID does not match base index")

// OpenFolder opens (or creates) the per-folder index. uidValidity is
// used only for a fresh folder; on an existing folder the on-disk value
// is authoritative. The returned Folder.ID keys all per-folder calls.
// A legacy-format .index is migrated atomically on first open, leaving
// a .legacy backup.
// openIntent says why a folder is being opened. The two answers differ in what
// a missing index means: for a folder being created it means "make one", and
// for a folder being opened it means "something that existed is not here".
// One call site was answering both, and it answered as if every folder were
// new (#1608).
type openIntent int

const (
	intentOpen openIntent = iota
	intentCreate
)

func (u *userIndex) OpenFolder(folder string, uidValidity uint32, traceID string) (*mailbox.Folder, error) {
	return u.openFolder(folder, uidValidity, traceID, intentOpen)
}

// CreateFolder makes a folder's index, and says so. It never looks for another
// implementation's index: a folder being created has no past to adopt.
func (u *userIndex) CreateFolder(folder string, uidValidity uint32, traceID string) (*mailbox.Folder, error) {
	return u.openFolder(folder, uidValidity, traceID, intentCreate)
}

func (u *userIndex) openFolder(folder string, uidValidity uint32, traceID string, intent openIntent) (*mailbox.Folder, error) {
	indexDir := u.indexDir(folder)
	indexPath := indexPathFor(indexDir)

	// Reuse an already-open folderState for the same (user, folder);
	// reload first so the snapshot reflects writes from other sessions.
	u.mu.Lock()
	if u.byDir != nil {
		if id, ok := u.byDir[indexDir]; ok {
			fsDedup := u.open[id]
			u.mu.Unlock()
			if traceID != "" && fsDedup != nil {
				fsDedup.mu.Lock()
				fsDedup.traceID = traceID
				fsDedup.mu.Unlock()
			}
			// Re-opening what this index already holds: a reload and a
			// snapshot, which is the same read FolderVSize makes lock-free
			// since #1635. A folder whose lineage cannot prove freshness falls
			// back to the locked read inside (#1639).
			var snap *mailbox.Folder
			err := u.withFolderROUnlocked(id, func(fs *folderState) error {
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

	// indexDir depends on u.driver via mailbox.FolderSubpath. A driver
	// mismatch would compute a different indexDir for the same folder and
	// register a disconnected folderState; log first opens to catch that.
	slog.Debug("fileindex: openfolder first-open, computing layout",
		"trace_id", traceID, "folder", folder, "driver", u.driver, "index_dir", indexDir)

	if u.b.noCreate {
		// Check before any mkdir: a no-create open must leave the filesystem
		// exactly as it found it, or a mis-resolved path still gets a directory
		// chain built under it.
		if _, statErr := os.Stat(indexPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, fmt.Errorf("fileindex/openfolder: no index at %s for folder %q: %w",
					indexPath, folder, os.ErrNotExist)
			}
			return nil, fmt.Errorf("fileindex/openfolder: stat %s: %w", indexPath, statErr)
		}
	} else if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return nil, fmt.Errorf("fileindex/openfolder: mkdir: %w", err)
	}
	switch err := migrateLegacyFilenames(indexDir); {
	case errors.Is(err, errForeignIndexPresent):
		// Theirs, under a name that was ours once. What happens next is the
		// driver's business: a dbox folder is converted from it and must keep
		// it until then, while a maildir folder is served from the files
		// themselves and never reads it -- so for maildir it is dead weight
		// that no tool of theirs should find and take for authoritative
		// (#1593).
		if u.driver == "maildir" {
			removeForeignIndexFiles(indexDir)
		}
	case err != nil:
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
		traceID:     traceID,
		intent:      intent,
	}
	if err := u.loadOrInit(fs, uidValidity); err != nil {
		return nil, err
	}
	if err := u.stampLineage(fs); err != nil {
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

// stampLineage gives a folder written before the lineage extension one, on the
// first open after the upgrade. Without it the property arrives with the next
// flush -- which on a read-only workload never happens, so a folder that is
// only ever read stays unable to prove its freshness forever and every
// "lock-free" read falls back to the locked path. That is exactly what the
// first measurement showed: adopt zero, acquisitions unchanged (#1229).
//
// One flush per folder, under the exclusive lock, announced once. The flush is
// the cheap part; being told it happened is what keeps a one-time cost from
// reading as a mystery in the logs.
func (u *userIndex) stampLineage(fs *folderState) error {
	fs.mu.RLock()
	known := fs.lineage.Lineage != lineageUnknown || fs.file == nil
	fs.mu.RUnlock()
	if known {
		return nil
	}
	return u.withFolderLock(fs, func() error {
		// Re-check under the lock: a racer may have stamped it, and a second
		// flush would rewrite a base nobody needed rewritten.
		if fs.lineage.Lineage != lineageUnknown {
			return nil
		}
		// Flush only. Truncating the log here would be a second, unrelated
		// decision, and a wrong one: a writer appending to it right now would
		// lose the entries it has already committed. The flush folds the log
		// into the base and records how far it reached, which is all the
		// pairing needs.
		if err := fs.flush(true); err != nil {
			return fmt.Errorf("fileindex/stamp: flush: %w", err)
		}
		// A log that holds nothing but its own header can be reissued under the
		// new lineage: there are no entries to lose, and we hold the exclusive
		// lock, so nobody is appending. Without this a folder whose log is a
		// bare stub -- written before the base was stamped, so announcing
		// nothing -- would stay unprovable until a compaction that a read-only
		// workload never performs. That is the same trap this whole change
		// exists to get out of.
		if lg, lerr := openLogRead(fs.indexPath); lerr == nil {
			headerOnly := lg.f != nil && lg.size <= int64(mailindex.LogHeaderSize)
			stale := lg.lineage() != fs.lineage.Lineage
			lg.close()
			// No floor stamp here, deliberately: this replaces a log that is a
			// header and nothing else, so no expunge record is being dropped.
			// Raising the floor would cost every reader a full resync for a
			// truncate that lost nothing.
			if headerOnly && stale {
				if err := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); err != nil {
					return fmt.Errorf("fileindex/stamp: reissue empty log: %w", err)
				}
				fs.logSize = 0
			}
		}
		metricLineageStamped.Inc()
		slog.Warn("fileindex: folder index written before the lineage extension, stamping it once",
			"user", u.username, "folder", fs.folder, "lineage", fs.lineage.Lineage)
		return nil
	})
}

// loadOrInit populates fs.file by reading the existing .index, migrating
// from legacy format, or creating a fresh file. The initial stat is
// unlocked on purpose: existing folders are the common case, and only
// the ErrNotExist branch needs the cross-process lock (see
// loadOrInitMissing).
func (u *userIndex) loadOrInit(fs *folderState, uidValidity uint32) error {
	st, err := os.Stat(fs.indexPath)
	// Log every stat outcome so cross-process stat-history for a path can
	// be reconstructed from the logs.
	if err != nil {
		slog.Debug("fileindex: loadOrInit stat", "trace_id", fs.traceID, "folder", fs.folder,
			"index_path", fs.indexPath, "exists", false, "err", err.Error())
	} else {
		slog.Debug("fileindex: loadOrInit stat", "trace_id", fs.traceID, "folder", fs.folder,
			"index_path", fs.indexPath, "exists", true, "size", st.Size(), "mod_time", st.ModTime().UnixNano())
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return u.loadOrInitMissing(fs, uidValidity)
	case err != nil:
		return fmt.Errorf("fileindex/openfolder: stat: %w", err)
	}
	_ = st
	if err := u.loadExisting(fs); err != nil {
		// An unreadable on-disk format is the state of the data, not a fault
		// in this code, and it stops at one folder. Named here so every layer
		// above can say WHICH folder without re-deriving it from a path.
		return asCorrupt(fs.folder, err)
	}
	return nil
}

// loadOrInitMissing handles the ErrNotExist branch under the folder's
// cross-process lock. Two racing openers may both see ErrNotExist from
// the unlocked stat; without the lock and the re-stat under it, the
// loser's createFresh would reset NextUID to 1 and discard the winner's
// committed UIDs.
func (u *userIndex) loadOrInitMissing(fs *folderState, uidValidity uint32) error {
	return u.withDistLock(fs, false, lockSiteOpenProbe, func() error {
		st, err := os.Stat(fs.indexPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if u.b.noCreate {
				return fmt.Errorf("fileindex/openfolder: no index at %s for folder %q: %w",
					fs.indexPath, fs.folder, os.ErrNotExist)
			}
			// Before deciding this folder is new: another implementation may
			// have written it, in which case its state is on disk in their
			// format and a fresh empty index would hide it (#1524).
			if fs.intent == intentCreate {
				// A folder being created has no past: nothing of theirs to
				// adopt into a name somebody just asked for.
				return fs.createFresh(u.newFolderUIDValidity(fs.folder, uidValidity))
			}
			switch converted, cerr := u.convertForeignFolder(fs); {
			case cerr != nil:
				return cerr
			case converted:
				return nil
			}
			if err := u.refuseIfIndexLost(fs); err != nil {
				return err
			}
			return fs.createFresh(u.identityFor(fs.folder, uidValidity))
		case err != nil:
			return fmt.Errorf("fileindex/openfolder: stat (locked recheck): %w", err)
		}
		_ = st
		return u.loadExisting(fs)
	})
}

// loadExisting populates fs.file from an index file already confirmed
// present on disk.
func (u *userIndex) loadExisting(fs *folderState) error {
	if _, isLegacy, err := detectAndDecodeLegacy(fs.indexPath); err != nil {
		return fmt.Errorf("fileindex/openfolder: legacy probe: %w", err)
	} else if isLegacy {
		// Legacy migration writes the index, so take the folder lock and
		// re-detect under it: a racer that already migrated wins and we load
		// the migrated file. Re-entrant via HoldsResource when the missing
		// branch already holds the lock.
		return u.withDistLock(fs, false, lockSiteOpenProbe, func() error {
			legacy, stillLegacy, err := detectAndDecodeLegacy(fs.indexPath)
			if err != nil {
				return fmt.Errorf("fileindex/openfolder: legacy probe (locked): %w", err)
			}
			if !stillLegacy {
				return u.loadModern(fs)
			}
			if err := fs.adoptLegacy(legacy); err != nil {
				return fmt.Errorf("fileindex/openfolder: adopt legacy: %w", err)
			}
			// Keep the old file as .legacy backup for manual rollback.
			backup := fs.indexPath + ".legacy"
			_ = os.Remove(backup)
			if err := os.Link(fs.indexPath, backup); err != nil {
				debugLog("legacy backup hardlink failed", "err", err)
			}
			if err := fs.flush(true); err != nil {
				return fmt.Errorf("fileindex/openfolder: write migrated: %w", err)
			}
			return ensureLogStub(fs.indexPath, fs.volatileDir, fs.file.Header.IndexID, fs.lineage.Lineage)
		})
	}
	return u.loadModern(fs)
}

// readBase opens the base .index into fs.file, records its mtime, and
// replays the .log (resetting a mismatched-IndexID log under the folder
// lock).
func (u *userIndex) readBase(fs *folderState) error {
	mf, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/openfolder: open: %w", err)
	}
	fs.file = mf
	// Every path that loads a base learns its pairing here, so "does this
	// folder have a lineage" is answered by the file rather than by which
	// function happened to load it.
	fs.lineage = readLineage(mf)
	if st, stErr := os.Stat(fs.indexPath); stErr == nil {
		fs.baseMod = st.ModTime()
		fs.baseIdent = st
	}
	// fs.logSize must come from applyLog's confirmed-applied offset, never
	// from an os.Stat around the call: a pre-call stat can under-report
	// (harmless re-apply), a post-call stat can over-report a concurrent
	// append applyLog never parsed, making reload's fast path skip it
	// forever.
	if _, logErr := os.Stat(fs.indexPath + ".log"); logErr == nil {
		confirmedEnd, applyErr := fs.applyLog(0)
		if errors.Is(applyErr, errLogIndexIDMismatch) {
			// Log belongs to a deleted/recreated mailbox; reset it under the
			// distributed lock so concurrent writers don't race the truncate.
			if lockErr := u.withFolderLock(fs, func() error {
				slog.Warn("fileindex: discarding log with mismatched IndexID on open",
					"folder", fs.folder)
				fs.closeFDs()
				// A zero lineage here is fine and intended, not a gap: the base
				// may predate the extension, and a log that pairs with nothing
				// simply sends readers back to the locked path.
				if truncErr := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); truncErr != nil {
					return fmt.Errorf("fileindex/openfolder: truncate after indexid mismatch: %w", truncErr)
				}
				fs.logSize = 0
				return nil
			}); lockErr != nil {
				return lockErr
			}
		} else if applyErr != nil {
			return fmt.Errorf("fileindex/openfolder: applylog: %w", applyErr)
		} else {
			fs.logSize = confirmedEnd
		}
	}
	return nil
}

// loadModern populates fs from a modern (non-legacy) index already present on
// disk, repairing a zero UIDVALIDITY if needed.
func (u *userIndex) loadModern(fs *folderState) error {
	if err := u.readBase(fs); err != nil {
		return err
	}
	if fs.file.Header.UIDValidity == 0 {
		// The UIDVALIDITY repair writes the index; serialize against other
		// openers and re-read under the lock so a racer that already repaired
		// it wins. Re-entrant via HoldsResource.
		if err := u.withDistLock(fs, false, lockSiteOpenProbe, func() error {
			if err := u.readBase(fs); err != nil {
				return err
			}
			if fs.file.Header.UIDValidity != 0 {
				return nil // a racer already repaired it
			}
			fs.file.Header.UIDValidity = uint32(time.Now().Unix())
			if err := fs.flush(true); err != nil {
				return fmt.Errorf("fileindex/openfolder: fix uidvalidity: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := fs.refreshExtState(); err != nil {
		return err
	}
	fs.ensureVsizeLocked()
	return ensureLogStub(fs.indexPath, fs.volatileDir, fs.file.Header.IndexID, fs.lineage.Lineage)
}

// createFresh initialises a brand-new folder state, used for first-ever
// OpenFolder and as the fallback after a corrupt file is moved aside.
func (fs *folderState) createFresh(uidValidity uint32) error {
	// This resets NextUID to 1; log the caller so an unexpected reset of an
	// established folder can be traced.
	if pc, _, _, ok := runtime.Caller(1); ok {
		caller := "unknown"
		if fn := runtime.FuncForPC(pc); fn != nil {
			caller = fn.Name()
		}
		slog.Warn("fileindex: createFresh resetting NextUID to 1",
			"trace_id", fs.traceID, "folder", fs.folder, "caller", caller, "requested_uidvalidity", uidValidity,
			"index_path", fs.indexPath, "index_dir", fs.indexDir)
	}
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
	return ensureLogStub(fs.indexPath, fs.volatileDir, indexID, fs.lineage.Lineage)
}

// refreshExtStateFromDisk re-reads the base file's extension HEADERS -- never
// its records -- and re-parses fs's typed copies from them. For the path that
// keeps the records it already has and must still pick up what the headers now
// say (see the adopt branch in reload).
func (fs *folderState) refreshExtStateFromDisk() error {
	exts, err := peekExtHeaders(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/refresh: peek extension headers: %w", err)
	}
	if len(exts) == 0 {
		return nil
	}
	// The typed copies come from the freshly read headers; fs.file keeps its
	// own extension list, which describes the record layout the records in
	// memory were decoded with.
	saved := fs.file.Extensions
	fs.file.Extensions = exts
	err = fs.refreshExtState()
	fs.file.Extensions = saved
	return err
}

// refreshExtState re-parses the dbox-hdr and keywords extension headers
// into fs's typed copies after every open or re-read.
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
	if ext := findExt(fs.file.Extensions, extNameHdrVsize); ext != nil && len(ext.HdrData) >= hdrVsizeSize {
		v, err := decodeHdrVsize(ext.HdrData)
		if err != nil {
			return fmt.Errorf("fileindex/refresh: hdr-vsize: %w", err)
		}
		fs.vsize = v
	}
	return nil
}

// recalcVsizeLocked recomputes the aggregate virtual size from the
// per-record vsize extension, falling back to the physical size for
// records that predate the extension. Caller holds fs.mu.
func (fs *folderState) recalcVsizeLocked() {
	var (
		total  uint64
		maxUID uint32
	)
	for _, rec := range fs.file.Records {
		v := decodeVsizeRec(rec.Ext[extNameVsize])
		if v == 0 {
			v = fs.sizes[rec.UID] // legacy record: best-available physical size
		}
		total += uint64(v)
		if rec.UID > maxUID {
			maxUID = rec.UID
		}
	}
	fs.vsize = hdrVsize{
		Vsize:        total,
		HighestUID:   maxUID,
		MessageCount: uint32(len(fs.file.Records)),
	}
}

// ensureVsizeLocked recomputes the aggregate only when the O(1) validity
// check (highest_uid+1 == uidnext && message_count == messages) says it
// is stale, so a quota read does not rescan every message. Caller holds
// fs.mu.
func (fs *folderState) ensureVsizeLocked() {
	if fs.vsize.MessageCount == fs.file.Header.MessagesCount &&
		fs.vsize.HighestUID+1 == fs.file.Header.NextUID {
		return
	}
	fs.recalcVsizeLocked()
}

// persistVsizeLocked writes the cached aggregate into the hdr-vsize
// extension header. Caller holds fs.mu.
func (fs *folderState) persistVsizeLocked() {
	data := encodeHdrVsize(fs.vsize)
	if ext := findExt(fs.file.Extensions, extNameHdrVsize); ext != nil {
		ext.HdrData = data
		ext.HdrSize = uint32(len(data))
		return
	}
	// Backfill the extension for base indexes that predate hdr-vsize.
	// AddHeaderExtension also fixes Header.HeaderSize; Recreate rejects a
	// header-size mismatch.
	if err := fs.file.AddHeaderExtension(extNameHdrVsize, data, 8, fs.file.Header.UIDValidity); err != nil {
		slog.Warn("fileindex: hdr-vsize backfill failed", "folder", fs.folder, "err", err)
	}
}

// extInventory renders the extension set for a log line: name, header size and
// record geometry, which is what a header-size disagreement is made of.
func extInventory(exts []mailindex.Extension) string {
	var b strings.Builder
	for i, e := range exts {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s(hdr=%d,rec=%d,align=%d)", e.Name, e.HdrSize, e.RecordSize, e.RecordAlign)
	}
	return b.String()
}

// snapshot returns a mailbox.Folder describing the current state.
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
		Fsckd:         fs.file.Header.Flags&mailindex.HdrFlagFsckd != 0,
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

// advanceModSeqAtLeast bumps highest_modseq to at least target; no-op
// when already >= target. Used when the caller pre-allocated a modseq
// via NextModSeq: the header must reflect it without bumping past it.
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

// flush rewrites the on-disk .index file from fs.file plus the .names
// sidecar from fs.filenames.
func (fs *folderState) flush(wholeNames bool) error {
	// flush persists Header.NextUID as ground truth and discards the log;
	// log the caller so a NextUID regression can be traced to the flush
	// that wrote it.
	if pc, _, _, ok := runtime.Caller(1); ok {
		caller := "unknown"
		if fn := runtime.FuncForPC(pc); fn != nil {
			caller = fn.Name()
		}
		slog.Debug("fileindex: flush persisting header",
			"trace_id", fs.traceID, "folder", fs.folder, "caller", caller, "next_uid", fs.file.Header.NextUID,
			"messages_count", fs.file.Header.MessagesCount)
	}
	if err := os.MkdirAll(fs.indexDir, 0o700); err != nil {
		return fmt.Errorf("fileindex/flush: mkdir: %w", err)
	}
	// Re-derive the vsize aggregate from records and persist it, mirroring
	// the message-count recount below.
	fs.recalcVsizeLocked()
	fs.persistVsizeLocked()
	// Mint the next lineage and record which log this base absorbs, and how far
	// into it, before the base is built from fs.file. A crash between the
	// rewrite and the log truncation then leaves a base that knows what it
	// already contains, rather than one the whole log is applied to again.
	prev := readLineage(fs.file)
	// Which log is being folded is read from the log, not assumed from our own
	// previous lineage: a log written before the extension carries the constant
	// the old code wrote, and assuming it carries ours would pair a base with a
	// log it never absorbed.
	folded := prev.Lineage
	if lg, lerr := openLogRead(fs.indexPath); lerr == nil {
		if seq := lg.lineage(); seq != lineageUnknown {
			folded = seq
		}
		lg.close()
	}
	next := lineageHdr{
		Lineage:       prev.Lineage + 1,
		FoldedLineage: folded,
		FoldedOffset:  uint64(fs.logSize),
		RecordsDigest: digestRecords(fs.file),
	}
	// Lineages start above the constant a pre-extension log carries, so a
	// stamped base can never claim that log as its own and replay what it has
	// already absorbed.
	if next.Lineage < legacyLogLineage+1 {
		next.Lineage = legacyLogLineage + 1
	}
	if err := setLineage(fs.file, next); err != nil {
		return fmt.Errorf("fileindex/flush: lineage: %w", err)
	}
	// One truth for the header size: whatever is about to be written decides
	// it, recomputed from those very extensions. Every path that grows an
	// extension header used to be responsible for recomputing it, and a path
	// that grew one without recomputing produced a base Recreate refuses --
	// with the folder then unflushable until someone noticed (#1285).
	if err := fs.syncHeaderSizeLocked(); err != nil {
		return err
	}
	ri := fs.file.ToRecreateInput(fs.indexPath)
	// Recount from actual records so counter drift is corrected on every
	// flush rather than persisted to the next base file.
	ri.Header.MessagesCount = uint32(len(ri.Records))
	ri.Header.SeenMessagesCount = 0
	ri.Header.DeletedMessagesCount = 0
	for _, rec := range ri.Records {
		if rec.Flags&mailindex.FlagSeen != 0 {
			ri.Header.SeenMessagesCount++
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			ri.Header.DeletedMessagesCount++
		}
	}
	fs.file.Header.MessagesCount = ri.Header.MessagesCount
	fs.file.Header.SeenMessagesCount = ri.Header.SeenMessagesCount
	fs.file.Header.DeletedMessagesCount = ri.Header.DeletedMessagesCount
	if fs.volatileDir != "" {
		if err := os.MkdirAll(fs.volatileDir, 0o700); err != nil {
			return fmt.Errorf("fileindex/flush: mkdir volatile: %w", err)
		}
		ri.TmpDir = fs.volatileDir
	}
	// Durability is off by default: a folder flush is one of many, and the
	// rename is atomic, so a lost tail is re-derived from the log. The
	// conversion path is the exception -- it removes the only other copy of
	// this state immediately afterwards, so the bytes have to be on the disk
	// and not merely written (#1524).
	ri.Fsync = fs.fsyncOnFlush
	if _, err := mailindex.Recreate(ri); err != nil {
		// A rejected base is unwritable until someone can see WHY: the sizes in
		// the error name a disagreement without naming which extension carries
		// it, and the state that produced it is gone by the time anyone reads
		// the log (#1285). The inventory is what turns the next occurrence into
		// a diagnosis instead of another reproduction attempt.
		slog.Error("fileindex: base rewrite refused; the folder cannot be flushed",
			"folder", fs.folder, "err", err,
			"header_size", ri.Header.HeaderSize, "record_size", ri.Header.RecordSize,
			"extensions", extInventory(ri.Extensions),
			"keyword_names", len(fs.keywords.Names))
		return fmt.Errorf("fileindex/flush: recreate: %w", err)
	}
	fs.lineage = next
	if wholeNames {
		if fs.namesFD != nil {
			_ = fs.namesFD.Close()
			fs.namesFD = nil
		}
		if err := saveNames(fs.indexDir, fs.volatileDir, fs.filenames, fs.sizes); err != nil {
			return err
		}
	}
	// Track base mtime+identity so the reload fast path fires after this flush.
	if st, _ := os.Stat(fs.indexPath); st != nil {
		fs.baseMod = st.ModTime()
		fs.baseIdent = st
	}
	return nil
}

// withFolder locates folderID's state, locks it, reloads the on-disk
// snapshot, and runs fn. fn sees the freshest committed state and must
// flush its own mutations. "file does not exist" from reload is
// swallowed so the caller can still createFresh.
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

// reload rereads the on-disk state into fs. Caller MUST hold the folder
// lock (exclusive for writers, shared for readers), so a concurrent
// locked compaction cannot leave fs with a torn view. Stages:
//
//  1. Neither base .index nor .index.log changed: return immediately.
//  2. Base unchanged, log grew: apply only the new log entries.
//  3. Base changed: full re-read of base + remaining log.
func (fs *folderState) reload() error {
	// One wrapper for every path out of the read: the format is unreadable in
	// more places than the first open, and the field found it through this one
	// (#1344). Naming the folder at the point of failure is what lets the
	// layers above answer per folder instead of per account.
	return asCorrupt(fs.folder, fs.reloadLocked())
}

// asCorrupt names the folder on an error that means what is on disk is not
// what this version reads. It is the state of the data, not a fault in this
// code, and it stops at one folder.
func asCorrupt(folder string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mailindex.ErrMajorMismatch) || errors.Is(err, mailindex.ErrShortRead) ||
		errors.Is(err, mailindex.ErrEndian) {
		return &mailbox.CorruptIndexError{Folder: folder, Err: err}
	}
	return err
}

func (fs *folderState) reloadLocked() error {
	t0 := time.Now()
	nextUIDBefore := uint32(0)
	if fs.file != nil {
		nextUIDBefore = fs.file.Header.NextUID
	}
	baseStat, baseErr := os.Stat(fs.indexPath)

	// Open the log ONCE and take its identity, size, header and body from that
	// one descriptor. Reading the header by path and the body by another open
	// leaves a window: a sibling's compaction between the two makes the pairing
	// describe one file while the replay reads another, and the replay then
	// starts at an offset that means nothing in the file it is reading. The
	// lock used to exclude that; a lock-free reader has to exclude it by
	// construction.
	lg, lgErr := openLogRead(fs.indexPath)
	if lgErr != nil {
		return fmt.Errorf("fileindex/reload: log: %w", lgErr)
	}
	defer lg.close()
	logStat := lg.stat
	newLogSize := lg.size
	logReplaced := false
	if fs.logFD != nil && logStat != nil && !fdMatchesFile(fs.logFD, logStat) {
		logReplaced = true
		slog.Warn("fileindex: .log replaced under open fd, dropping stale handle",
			"folder", fs.folder)
		// closeFDs also drops namesFD: the same compaction rewrote the
		// .names sidecar, so the cached fd is stale too. Both reopen lazily.
		fs.closeFDs()
	}

	var newBaseMod time.Time
	if baseStat != nil {
		newBaseMod = baseStat.ModTime()
	}

	// Base identity check: mtime resolution is coarse on some filesystems,
	// so a same-tick replace of the base .index could be missed.
	// baseReplaced is true only when both stats are known and differ; an
	// unknown identity falls back to the mtime comparison below.
	baseReplaced := fs.baseIdent != nil && baseStat != nil && !os.SameFile(fs.baseIdent, baseStat)

	// Fast path: nothing on disk changed. Never taken when the log was
	// replaced: that means a concurrent compaction rewrote the base too,
	// and its new mtime may coincide with the cached one.
	if !logReplaced && !baseReplaced && newBaseMod == fs.baseMod && newLogSize == fs.logSize {
		slog.Debug("fileindex: reload fast-path",
			"trace_id", fs.traceID, "folder", fs.folder,
			"log_size", fs.logSize,
			"base_mod", fs.baseMod.UnixNano(),
			"next_uid", nextUIDBefore,
			"dur_ms", time.Since(t0).Milliseconds())
		return nil
	}
	recordsBefore := 0
	if fs.file != nil {
		recordsBefore = len(fs.file.Records)
	}
	slog.Debug("fileindex: reload full",
		"trace_id", fs.traceID, "folder", fs.folder,
		"new_log_size", newLogSize,
		"old_log_size", fs.logSize,
		"new_base_mod", newBaseMod.UnixNano(),
		"old_base_mod", fs.baseMod.UnixNano(),
		"next_uid_before", nextUIDBefore,
		"dur_ms", time.Since(t0).Milliseconds())

	// Base file changed (or first open, or the log was replaced): full reload.
	if newBaseMod != fs.baseMod || baseReplaced || fs.file == nil || logReplaced {
		if baseErr != nil {
			return fmt.Errorf("fileindex/reload: %w", baseErr)
		}
		// A rewritten base often holds exactly what this handle already holds:
		// a compaction folds in the log we already applied. Reading the header
		// alone answers that, and the digest proves it rather than assuming it
		// -- several paths rewrite the base while folding the same log, so the
		// offsets agreeing is not enough (#1228 learned this the hard way).
		if fs.file != nil && !logReplaced {
			if h, perr := peekLineage(fs.indexPath); perr == nil && h.Lineage != lineageUnknown &&
				h.FoldedLineage == fs.lineage.Lineage && uint64(fs.logSize) >= h.FoldedOffset &&
				h.RecordsDigest == digestRecords(fs.file) {
				fs.lineage = h
				fs.baseMod = newBaseMod
				fs.baseIdent = baseStat
				fs.logSize = 0
				// The records are the ones we hold -- that is what the digest
				// proved -- but the extension HEADERS are not covered by it,
				// and a base is rewritten for them alone: registering a
				// keyword name changes no record. Keeping the stale registry
				// here made a bitmask bit set by another process decode to no
				// name at all, so a custom keyword set over IMAP was invisible
				// over JMAP (#1278).
				if err := fs.refreshExtStateFromDisk(); err != nil {
					return err
				}
				metricReload.WithLabelValues("adopt").Inc()
				return fs.applyLogTail(lg)
			}
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
		fs.baseIdent = baseStat
		fs.lineage = readLineage(mf)
		// Where to resume in the log the base did not fully absorb. Without the
		// pairing this restarts from zero and relies on every transaction type
		// being idempotent -- which they are today, but that is a property
		// nobody declared and the next transaction type need not have.
		fs.logSize = 0
		if off, paired := replayStart(fs.lineage, lg.lineage()); paired {
			fs.logSize = off
		}
	}

	if err := fs.applyLogTail(lg); err != nil {
		return err
	}
	fs.ensureVsizeLocked()
	// Report how the record set changed so a "message not visible after
	// delivery" case shows whether the record was picked up.
	slog.Debug("fileindex: reload applied",
		"trace_id", fs.traceID, "folder", fs.folder,
		"records_before", recordsBefore,
		"records_after", len(fs.file.Records),
		"next_uid_before", nextUIDBefore,
		"next_uid_after", fs.file.Header.NextUID,
		"log_size", fs.logSize,
		"dur_ms", time.Since(t0).Milliseconds())
	return nil
}

// applyLogTail folds in whatever the log gained past what this handle has
// applied. Split out because both the full reload and the adopt path end here:
// taking a new base is never the end of a refresh, since the writer that
// produced it may already have appended to the log it started.
func (fs *folderState) applyLogTail(lg *logReader) error {
	if lg.size > fs.logSize {
		// fs.logSize comes from applyLog's confirmed-applied return value,
		// not the pre-call stat (see readBase). If an append landed mid-read,
		// the next reload re-applies the remainder.
		if confirmedEnd, err := fs.applyLogFrom(lg, fs.logSize); errors.Is(err, errLogIndexIDMismatch) {
			// Stale log from a previous mailbox at this path: flush the
			// current base and reset the log.
			slog.Warn("fileindex: discarding log with mismatched IndexID, re-flushing base",
				"folder", fs.folder)
			// Conservative: the log belonged to a different mailbox at this
			// path, so its expunges were never ours -- but a reader cannot tell
			// that from the outside, and raising the floor costs a resync while
			// leaving it costs a phantom message.
			if floorErr := fs.stampExpungeFloorLocked(); floorErr != nil {
				return fmt.Errorf("fileindex/reload: stamp floor after indexid mismatch: %w", floorErr)
			}
			if flushErr := fs.flush(false); flushErr != nil {
				return fmt.Errorf("fileindex/reload: flush after indexid mismatch: %w", flushErr)
			}
			if truncErr := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); truncErr != nil {
				return fmt.Errorf("fileindex/reload: truncate after indexid mismatch: %w", truncErr)
			}
			fs.logSize = 0
		} else if err != nil {
			return err
		} else {
			fs.logSize = confirmedEnd
		}
	}
	return nil
}

// SaveFolder persists header-level mutations from f back to disk.
// Record-state changes are ignored; callers use AppendMessage /
// UpdateFlags / ExpungeMessage for those.
func (u *userIndex) SaveFolder(f *mailbox.Folder) error {
	return u.withFolder(f.ID, func(fs *folderState) error {
		return fs.flush(false)
	})
}

// AdoptUIDSpace sets a folder's UIDVALIDITY and next UID from a store that
// already records them, and refuses a folder that holds messages.
//
// The refusal is the substance. Changing the UID space of a mailbox somebody is
// already reading is what UIDVALIDITY exists to prevent, and a folder with
// records is one a session may have seen; a folder with none was created a
// moment ago by the open that is adopting it.
func (u *userIndex) AdoptUIDSpace(folderID uint64, uidValidity, nextUID uint32) error {
	if uidValidity == 0 {
		return fmt.Errorf("fileindex/adopt: uid validity 0")
	}
	return u.withFolder(folderID, func(fs *folderState) error {
		if len(fs.file.Records) > 0 {
			return fmt.Errorf("fileindex/adopt: folder %q holds %d messages: %w",
				fs.folder, len(fs.file.Records), mailbox.ErrUIDSpaceInUse)
		}
		slog.Info("fileindex: adopting a recorded uid space", "user", u.username,
			"folder", fs.folder, "uid_validity", uidValidity, "next_uid", nextUID,
			"was_uid_validity", fs.file.Header.UIDValidity)
		fs.file.Header.UIDValidity = uidValidity
		if nextUID > fs.file.Header.NextUID {
			fs.file.Header.NextUID = nextUID
		}
		return fs.flush(false)
	})
}

// AppendMessage records m as a new on-disk record. The caller is
// expected to have already assigned m.UID via AllocateUID or via
// an external authority (mdbox-style map_uid).
func (u *userIndex) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	if err := u.withFolder(folderID, func(fs *folderState) error {
		// next_uid_before exposes a UID-reuse race in the logs: a commit with
		// UID < next_uid_before means the counter advanced past this UID
		// since AllocateUID ran.
		slog.Debug("fileindex: committing pre-allocated uid", "trace_id", fs.traceID,
			"user", u.username, "folder", fs.folder, "uid", m.UID, "next_uid_before", fs.file.Header.NextUID)
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
	return nil
}

// FolderVSize returns the folder's aggregate virtual size and message count
// from the hdr-vsize cache, which is what the count quota backend sums. A read,
// taken as one: through the write path it cost an exclusive lock per folder,
// for every folder, before every save (#1634).
func (u *userIndex) FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error) {
	err = u.withFolderROUnlocked(folderID, func(fs *folderState) error {
		bytes = fs.vsize.Vsize
		messages = fs.vsize.MessageCount
		return nil
	})
	return bytes, messages, err
}

// RecomputeVSize forces a full rebuild of the folder's hdr-vsize aggregate from
// the per-record vsize extension and persists it, bypassing the validity check.
// The admin recovery path for a corrupted aggregate; normal reads self-heal.
func (u *userIndex) RecomputeVSize(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		fs.recalcVsizeLocked()
		fs.persistVsizeLocked()
		return fs.flush(false)
	})
}

// GUIDBackfillNeeded reads the guid extension header. An index predating the
// extension has no header at all and decodes as pending, which is exactly the
// set of folders that still carry zero GUIDs.
func (u *userIndex) GUIDBackfillNeeded(folderID uint64) (bool, error) {
	var need bool
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		ext := findExt(fs.file.Extensions, extNameGUID)
		need = ext == nil || decodeGUIDHdr(ext.HdrData) != guidStateComplete
		return nil
	})
	return need, err
}

// SetGUIDs stamps storage-provided GUIDs onto records that have none and flips
// the header to complete. Records already carrying a GUID are left alone, so an
// interrupted pass resumes to the same result and a second run changes nothing.
func (u *userIndex) SetGUIDs(folderID uint64, guids map[uint32][16]byte) error {
	var zero [16]byte
	return u.withFolder(folderID, func(fs *folderState) error {
		// An index written before the extension existed needs it added first;
		// existing records gain 16 zero bytes on the next write.
		if findExt(fs.file.Extensions, extNameGUID) == nil {
			if err := fs.file.AddRecordExtension(extNameGUID, encodeGUIDHdr(guidStatePending),
				guidRecSize, 1, fs.file.Header.UIDValidity); err != nil {
				return fmt.Errorf("fileindex: add guid extension: %w", err)
			}
		}
		for _, rec := range fs.file.Records {
			g, ok := guids[rec.UID]
			if !ok || g == zero {
				continue
			}
			if decodeGUIDRec(rec.Ext[extNameGUID]) != zero {
				continue // already stamped: never rewrite an assigned identity
			}
			if rec.Ext == nil {
				rec.Ext = make(map[string][]byte, 1)
			}
			rec.Ext[extNameGUID] = encodeGUIDRec(g)
		}
		if ext := findExt(fs.file.Extensions, extNameGUID); ext != nil {
			ext.HdrData = encodeGUIDHdr(guidStateComplete)
			ext.HdrSize = guidHdrSize
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
		// Pairs with the "committing pre-allocated uid" log in AppendMessage;
		// the gap between them is the caller's Save() window.
		slog.Debug("fileindex: uid allocated", "trace_id", fs.traceID, "user", u.username, "folder", fs.folder, "uid", assigned)
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
	if err := u.withFolder(folderID, func(fs *folderState) error {
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
	return nil
}

// appendLocked is the in-memory half of AppendMessage. Caller must hold
// the folder lock. Non-zero m.ModSeq is a pre-allocated value and is
// recorded as-is (advancing the high-watermark only if needed); zero
// means bump the counter and write the new value into m.
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
	// Keyword registry grew: persist extension headers to the base file so
	// a cross-pod reader can decode keyword bitmasks. Rare write (first use
	// of each keyword name only).
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
			extNameVsize:        encodeVsizeRec(m.RFC822Size()),
			extNameGUID:         encodeGUIDRec(m.GUID),
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
	fs.vsize.Vsize += uint64(m.RFC822Size())
	fs.vsize.MessageCount++
	if m.UID > fs.vsize.HighestUID {
		fs.vsize.HighestUID = m.UID
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
	return u.writeFlags(folderID, uid, flags, keywords, flagsReplace)
}

// AddFlags adds flags and keywords to a message, keeping whatever it already
// carries. The union is computed from the record as the lock finds it, which is
// the difference that matters: UpdateFlags writes an absolute list, so a caller
// that built one from an earlier read overwrites every change made in between.
// The implicit \Seen of a non-PEEK FETCH is exactly that caller (#1250).
func (u *userIndex) AddFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return u.writeFlags(folderID, uid, flags, keywords, flagsAdd)
}

// RemoveFlags clears flags and keywords from a message, leaving the rest as the
// lock finds them. The counterpart of AddFlags, and needed for the same reason:
// a caller clearing one flag through UpdateFlags has to send the whole
// remaining set, which is a set it read earlier.
func (u *userIndex) RemoveFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return u.writeFlags(folderID, uid, flags, keywords, flagsRemove)
}

// flagWriteMode selects what writeFlags does with the flags it is given.
type flagWriteMode int

const (
	flagsReplace flagWriteMode = iota
	flagsAdd
	flagsRemove
)

// writeFlags is the shared body: replace the flag set, union with it, or
// subtract from it.
func (u *userIndex) writeFlags(folderID uint64, uid uint32, flags, keywords []string, mode flagWriteMode) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		modseq, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		// Read the record's own keywords under the lock: Add/Remove fold the
		// caller's list into them, so a keyword set between the caller's read
		// and this write is not dropped by a list that predates it, and every
		// mode needs them anyway to journal the difference.
		var have []string
		for _, rec := range fs.file.Records {
			if rec.UID != uid {
				continue
			}
			have = keywordsFromBitmask(fs.keywords, decodeKeywordsRec(rec.Ext[extNameKeywords]))
			break
		}
		if mode == flagsAdd {
			keywords = unionStrings(have, keywords)
		} else if mode == flagsRemove {
			keywords = subtractStrings(have, keywords)
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
			// Preserve the backend-private AltTier bit; IMAP STORE must not
			// clear a tier marker it knows nothing about.
			newFlags |= rec.Flags & mailindex.FlagBackend
			switch mode {
			case flagsAdd:
				newFlags |= rec.Flags
			case flagsRemove:
				newFlags = rec.Flags &^ mailindex.MailFlag(imapFlagsToIndex(flags))
			}
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
		recs := []([]byte){
			encLogRec(mailindex.TxTypeModseqUpdate, 0, mailindex.EncodeTxModseqUpdatePayload([]mailindex.TxModseqUpdate{{
				UID: uid, ModSeqLow32: uint32(modseq), ModSeqHigh32: uint32(modseq >> 32),
			}})),
			encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload([]mailindex.TxFlagUpdate{{
				UID1: uid, UID2: uid, AddFlags: newFlags, RemoveFlags: ^newFlags,
			}})),
		}
		recs = append(recs, keywordLogRecords(uid, have, keywords)...)
		recs = append(recs,
			encU32Update(40, fs.file.Header.SeenMessagesCount),
			encU32Update(44, fs.file.Header.DeletedMessagesCount),
		)
		return fs.appendMutLog(recs...)
	})
}

// UpdateFilename repoints the stored on-disk filename for a UID. The
// filename lives only in the .names sidecar; last write wins on reload.
// No-op when uid is unknown.
func (u *userIndex) UpdateFilename(folderID uint64, uid uint32, filename string) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if _, ok := fs.filenames[uid]; !ok {
			return nil
		}
		if fs.filenames[uid] == filename {
			return nil
		}
		fs.filenames[uid] = filename
		return fs.appendName(uid, filename, fs.sizes[uid])
	})
}

// MarkFolderCorrupt persists the FSCKD header flag (header offset 20) so
// the next open triggers a reactive rebuild. Idempotent.
func (u *userIndex) MarkFolderCorrupt(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if fs.file.Header.Flags&mailindex.HdrFlagFsckd != 0 {
			return nil
		}
		fs.file.Header.Flags |= mailindex.HdrFlagFsckd
		return fs.appendMutLog(encU32Update(20, uint32(fs.file.Header.Flags)))
	})
}

// ClearFolderCorrupt clears the FSCKD marker after a successful rebuild.
func (u *userIndex) ClearFolderCorrupt(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if fs.file.Header.Flags&mailindex.HdrFlagFsckd == 0 {
			return nil
		}
		fs.file.Header.Flags &^= mailindex.HdrFlagFsckd
		return fs.appendMutLog(encU32Update(20, uint32(fs.file.Header.Flags)))
	})
}

// UpdateFlagsMulti replaces flags+keywords for a batch of UIDs in a single
// lock/reload/flush cycle. Each UID gets an individual modseq bump so clients
// can use CONDSTORE to pinpoint which messages changed. Returns the new modseq
// per UID; UIDs not found in the index are silently skipped.
func (u *userIndex) UpdateFlagsMulti(folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsResult, error) {
	result := make(map[uint32]mailbox.FlagsResult, len(updates))
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
		var keywordRecs [][]byte
		for _, rec := range fs.file.Records {
			upd, ok := updates[rec.UID]
			if !ok {
				continue
			}
			modseq, err := fs.bumpModSeqHeader()
			if err != nil {
				return err
			}
			// Under Add/Remove the caller named only what changes, so the set
			// is resolved here, against the record the lock is holding. A set
			// computed by the caller would be as old as its last read.
			kwWanted := upd.Keywords
			have := keywordsFromBitmask(fs.keywords, decodeKeywordsRec(rec.Ext[extNameKeywords]))
			if upd.Mode == mailbox.FlagsAdd {
				kwWanted = unionStrings(have, upd.Keywords)
			} else if upd.Mode == mailbox.FlagsRemove {
				kwWanted = subtractStrings(have, upd.Keywords)
			}
			kwBits, kwReg2, err := keywordsBitmaskFor(fs.keywords, kwWanted)
			if err != nil {
				return err
			}
			fs.keywords = kwReg2
			newFlags := mailindex.MailFlag(imapFlagsToIndex(upd.Flags))
			oldSeen := rec.Flags&mailindex.FlagSeen != 0
			oldDel := rec.Flags&mailindex.FlagDeleted != 0
			switch upd.Mode {
			case mailbox.FlagsAdd:
				newFlags |= rec.Flags
			case mailbox.FlagsRemove:
				newFlags = rec.Flags &^ newFlags
			}
			newFlags |= rec.Flags & mailindex.FlagBackend
			rec.Flags = newFlags
			rec.Ext[extNameModSeq] = encodeModseqRec(modseq)
			rec.Ext[extNameKeywords] = encodeKeywordsRec(kwBits)
			keywordRecs = append(keywordRecs, keywordLogRecords(rec.UID, have, kwWanted)...)
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
			result[rec.UID] = mailbox.FlagsResult{
				ModSeq:   modseq,
				Flags:    indexFlagsToIMAP(uint8(newFlags)),
				Keywords: kwWanted,
			}
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
		recs := []([]byte){
			encLogRec(mailindex.TxTypeModseqUpdate, 0, mailindex.EncodeTxModseqUpdatePayload(modseqUpdates)),
			encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload(flagUpdates)),
		}
		recs = append(recs, keywordRecs...)
		recs = append(recs,
			encU32Update(40, fs.file.Header.SeenMessagesCount),
			encU32Update(44, fs.file.Header.DeletedMessagesCount),
		)
		return fs.appendMutLog(recs...)
	})
	return result, err
}

// ExpungeMessage removes a record: writes a TxTypeExpungeGUID log entry
// (with EXPUNGE_PROT) and drops the in-memory record. Vanished later
// reads those log entries to satisfy QRESYNC.
func (u *userIndex) ExpungeMessage(folderID uint64, uid uint32) error {
	if err := u.withFolder(folderID, func(fs *folderState) error {
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
			return nil // already expunged
		}
		rec := fs.file.Records[idx]
		if rec.Flags&mailindex.FlagSeen != 0 {
			fs.file.Header.SeenMessagesCount--
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			fs.file.Header.DeletedMessagesCount--
		}
		expungedVSize := decodeVsizeRec(rec.Ext[extNameVsize])
		if expungedVSize == 0 {
			// Record without the per-record vsize extension: fall back to the
			// physical size, matching recalcVsizeLocked, so the aggregate is
			// not decremented by 0 and left stale.
			expungedVSize = fs.sizes[rec.UID]
		}
		fs.file.Records = append(fs.file.Records[:idx], fs.file.Records[idx+1:]...)
		fs.file.Header.MessagesCount--
		if uint64(expungedVSize) <= fs.vsize.Vsize {
			fs.vsize.Vsize -= uint64(expungedVSize)
		} else {
			fs.vsize.Vsize = 0
		}
		if fs.vsize.MessageCount > 0 {
			fs.vsize.MessageCount--
		}
		delete(fs.filenames, uid)
		delete(fs.sizes, uid)

		// 28-byte payload: uid(4)+guid(16)+modseq(8). Compatible with
		// scanExpungesSince which reads the same layout.
		expPayload := make([]byte, 28)
		le := binary.LittleEndian
		le.PutUint32(expPayload[0:], uid)
		// The MESSAGE's GUID, not the mailbox's. The field is the expunged
		// message's identity: it is the only place that identity survives,
		// because the record it came from is being removed. Writing the mailbox
		// GUID here gave every expunge in a folder the same value, which
		// QRESYNC never noticed -- it matches by UID -- and which a protocol
		// addressing messages by id cannot use at all (#1216).
		msgGUID := decodeGUIDRec(rec.Ext[extNameGUID])
		copy(expPayload[4:20], msgGUID[:])
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
	return nil
}

// GetMessages returns every record whose UID falls in uids; empty uids
// means all records. Output is sorted by UID ascending.
func (u *userIndex) GetMessages(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return u.getMessages(folderID, uids, false)
}

// GetMessagesUnlocked answers without the cross-process lock where the files can
// prove their own consistency. For readers whose answer goes to a client and
// decides nothing on disk -- FETCH, SEARCH, SELECT, STATUS, POLL. A caller whose
// answer drives a write or a delete must use GetMessages (#1249).
func (u *userIndex) GetMessagesUnlocked(folderID uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return u.getMessages(folderID, uids, true)
}

func (u *userIndex) getMessages(folderID uint64, uids mailbox.SeqSet, unlocked bool) ([]*mailbox.MessageMeta, error) {
	var out []*mailbox.MessageMeta
	read := u.withFolderRO
	if unlocked {
		read = u.withFolderROUnlocked
	}
	err := read(folderID, func(fs *folderState) error {
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
			if data, ok := rec.Ext[extNameCache]; ok {
				meta.CacheOffset = decodeCacheRec(data)
			}
			if data, ok := rec.Ext[extNameInternalDate]; ok {
				meta.InternalDate = decodeIdateRec(data)
			}
			if data, ok := rec.Ext[extNameGUID]; ok {
				meta.GUID = decodeGUIDRec(data)
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

// NextModSeq bumps highest_modseq and returns the post-bump value. Used
// by CONDSTORE writers that claim a modseq before writing the change.
func (u *userIndex) NextModSeq(folderID uint64) (uint64, error) {
	var out uint64
	err := u.withFolder(folderID, func(fs *folderState) error {
		v, err := fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		out = v
		// No flush needed; the subsequent AppendMessage / UpdateFlags
		// TxModseqUpdate log record persists the modseq.
		return nil
	})
	return out, err
}

// Vanished returns every UID expunged from this folder with expunge
// modseq strictly greater than sinceModSeq. Drives the QRESYNC VANISHED
// response (RFC 7162).
func (u *userIndex) Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	return u.vanished(folderID, sinceModSeq, false)
}

// VanishedUnlocked is Vanished for a caller whose answer goes to the client and
// decides nothing on disk — QRESYNC on SELECT, CHANGEDSINCE on FETCH (#1249).
func (u *userIndex) VanishedUnlocked(folderID uint64, sinceModSeq uint64) ([]uint32, error) {
	return u.vanished(folderID, sinceModSeq, true)
}

func (u *userIndex) vanished(folderID uint64, sinceModSeq uint64, unlocked bool) ([]uint32, error) {
	var out []uint32
	read := u.withFolderRO
	if unlocked {
		read = u.withFolderROUnlocked
	}
	err := read(folderID, func(fs *folderState) error {
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
	return u.keywords(folderID, false)
}

// KeywordsUnlocked is Keywords for the SELECT response: a keyword declared a
// moment later shows up on the next command, which is the staleness the
// protocol already accepts (#1249).
func (u *userIndex) KeywordsUnlocked(folderID uint64) ([]string, error) {
	return u.keywords(folderID, true)
}

func (u *userIndex) keywords(folderID uint64, unlocked bool) ([]string, error) {
	var out []string
	read := u.withFolderRO
	if unlocked {
		read = u.withFolderROUnlocked
	}
	err := read(folderID, func(fs *folderState) error {
		out = append([]string(nil), fs.keywords.Names...)
		return nil
	})
	return out, err
}

// ResetFolder replaces every record with the supplied set (admin
// rebuild flow). Preserves UIDValidity + folder GUID + indexID; sets
// NextUID past max(records.UID). Returns the UIDs dropped by the reset
// so the caller can invalidate their FTS documents.
//
// Per-message modseq is preserved: a surviving record keeps its own
// ModSeq and highest_modseq is advanced to the max carried in; only a
// record with no modseq is stamped a fresh value. A rebuild that changes
// no record leaves the header untouched (nothing to signal to QRESYNC).
func (u *userIndex) ResetFolder(folderID uint64, records []*mailbox.MessageMeta) ([]uint32, error) {
	var expunged []uint32
	err := u.withFolder(folderID, func(fs *folderState) error {
		highest, err := fs.highestModSeq()
		if err != nil {
			return err
		}
		// UIDs present before the reset, to diff against the new set.
		before := make(map[uint32]struct{}, len(fs.file.Records))
		for _, rec := range fs.file.Records {
			before[rec.UID] = struct{}{}
		}

		fs.file.Records = fs.file.Records[:0]
		fs.filenames = make(map[uint32]string)
		fs.sizes = make(map[uint32]uint32)
		fs.file.Header.MessagesCount = 0
		fs.file.Header.SeenMessagesCount = 0
		fs.file.Header.DeletedMessagesCount = 0

		var maxUID uint32
		fresh := highest     // fresh modseq counter for records that carry none
		maxModseq := highest // header must reflect the greatest modseq present
		kept := make(map[uint32]struct{}, len(records))
		for _, m := range records {
			if m == nil || m.UID == 0 {
				continue
			}
			kwBits, kwReg, err := keywordsBitmaskFor(fs.keywords, m.Keywords)
			if err != nil {
				return err
			}
			fs.keywords = kwReg
			modseq := m.ModSeq
			if modseq == 0 {
				fresh++
				modseq = fresh
			}
			if modseq > maxModseq {
				maxModseq = modseq
			}
			rec := &mailindex.Record{
				UID:   m.UID,
				Flags: mailindex.MailFlag(imapFlagsToIndex(m.Flags)),
				Ext: map[string][]byte{
					extNameModSeq:   encodeModseqRec(modseq),
					extNameKeywords: encodeKeywordsRec(kwBits),
					extNameVsize:    encodeVsizeRec(m.RFC822Size()),
					extNameGUID:     encodeGUIDRec(m.GUID),
				},
			}
			fs.file.Records = append(fs.file.Records, rec)
			kept[m.UID] = struct{}{}
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
		if err := fs.advanceModSeqAtLeast(maxModseq); err != nil {
			return err
		}
		for uid := range before {
			if _, ok := kept[uid]; !ok {
				expunged = append(expunged, uid)
			}
		}
		if maxUID >= fs.file.Header.NextUID {
			fs.file.Header.NextUID = maxUID + 1
		}
		if err := fs.persistKeywordRegistry(); err != nil {
			return err
		}
		if err := fs.stampExpungeFloorLocked(); err != nil {
			return err
		}
		if err := fs.flush(true); err != nil {
			return err
		}
		// Truncate the log so stale TxAppend records don't resurface
		// when another process replays the log after ResetFolder.
		fs.closeFDs()
		if err := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); err != nil {
			return err
		}
		fs.logSize = 0
		// Log kept vs dropped counts so a "missing after rebuild" message can
		// be traced to the dropped set.
		slog.Debug("fileindex: reset folder",
			"folder", fs.folder,
			"records_before", len(before),
			"records_after", len(fs.file.Records),
			"dropped", len(expunged))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(expunged, func(i, j int) bool { return expunged[i] < expunged[j] })
	return expunged, nil
}

// SetAltTier sets or clears FlagBackend on every record whose Filename
// is in the filenames set. Called after AltMove relocates m.<N> files so
// Fetch skips the primary open() for cold-tier messages.
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

// OptimizeIndex compacts pending log records into the base file and
// truncates the log. Afterwards Vanished(sinceModSeq) returns empty for
// every sinceModSeq < currentHighest: prior expunges have been absorbed
// into the base index.
func (u *userIndex) OptimizeIndex(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if err := fs.stampExpungeFloorLocked(); err != nil {
			return err
		}
		if err := fs.flush(true); err != nil {
			return err
		}
		fs.closeFDs()
		if err := truncateLogLineage(fs.indexPath, fs.file.Header.IndexID, fs.lineage.Lineage); err != nil {
			return err
		}
		// Log is now an empty header; next reload fast-paths the base and
		// applies zero log records.
		fs.logSize = 0
		return nil
	})
}

// VanishedGUIDs is Vanished by message identity rather than by UID: the ids a
// GUID-addressed protocol has to report as destroyed.
//
// complete is false when a record in range cannot be named. Expunges written
// before the field carried the message GUID hold the MAILBOX's instead, and
// those are indistinguishable from a real id except by that equality -- so they
// are dropped and the caller is told the answer is partial, rather than handed
// an id that names a mailbox and no message.
func (u *userIndex) VanishedGUIDs(folderID uint64, sinceModSeq uint64) (guids [][16]byte, complete bool, err error) {
	complete = true
	err = u.withFolder(folderID, func(fs *folderState) error {
		found, scanErr := scanExpungedGUIDsSince(fs.indexPath, sinceModSeq)
		if scanErr != nil {
			return scanErr
		}
		for _, g := range found {
			if g == fs.hdr.MailboxGUID || g == ([16]byte{}) {
				complete = false
				continue
			}
			guids = append(guids, g)
		}
		return nil
	})
	return guids, complete, err
}

// FolderStamp stats the folder's two files without opening it, which is the
// point: a caller holding a cached marker pays two stats instead of a base read
// and a log replay.
//
// A missing file reports as size -1, the same convention JournalSizes uses, so
// "not there" and "empty" stay different states.
func (u *userIndex) FolderStamp(folder string) (mailbox.FolderStamp, error) {
	indexPath := indexPathFor(u.indexDir(folder))
	stamp := mailbox.FolderStamp{BaseSize: -1, LogSize: -1}
	if st, err := os.Stat(indexPath); err == nil {
		stamp.BaseSize, stamp.BaseMod = st.Size(), st.ModTime()
	}
	if st, err := os.Stat(indexPath + ".log"); err == nil {
		stamp.LogSize, stamp.LogMod = st.Size(), st.ModTime()
	}
	return stamp, nil
}

// ExpungeFloor reports the modseq below which this folder can no longer answer
// "what was expunged since". Zero means nothing has been folded away yet, so
// the log still holds the whole history.
//
// A caller asking about a point below the floor must degrade -- a fresh listing
// for JMAP, an empty VANISHED (EARLIER) for QRESYNC -- rather than read the
// empty answer as "nothing was deleted" (#1216).
func (u *userIndex) ExpungeFloor(folderID uint64) (uint64, error) {
	var floor uint64
	err := u.withFolder(folderID, func(fs *folderState) error {
		floor = fs.expungeFloorLocked()
		return nil
	})
	return floor, err
}

// JournalSizes reports the on-disk size of the folder's base index and of its
// transaction log, as the filesystem answers right now. A log that does not
// exist reports -1, which is a state the drivers reach on purpose (the mdbox map
// folds by removing its log) and must not be reported as an empty one.
//
// Measured here rather than by the caller: the paths are this package's, and a
// caller reconstructing them would drift the moment index_dir or the layout
// changes, reporting sizes of files nobody folded.
func (u *userIndex) JournalSizes(folderID uint64) (int64, int64, error) {
	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return 0, 0, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	return fileSize(fs.indexPath), fileSize(fs.indexPath + ".log"), nil
}

// fileSize is the size of path, or -1 when it cannot be stat'd.
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

// persistKeywordRegistry encodes fs.keywords back into the keywords
// extension's HdrData.
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

// syncHeaderSizeLocked recomputes Header.HeaderSize from the extension headers
// as they stand, the same way Recreate validates it: base size plus the encoded
// extension region. Cheap (an encode of the header region, not of the records)
// and idempotent, so it is a barrier every write passes rather than a rule
// every writer has to remember.
func (fs *folderState) syncHeaderSizeLocked() error {
	extBytes, err := mailindex.EncodeExtHeaders(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex: encode extension headers: %w", err)
	}
	want := uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if fs.file.Header.HeaderSize != want {
		slog.Debug("fileindex: header size corrected before flush",
			"folder", fs.folder, "was", fs.file.Header.HeaderSize, "now", want)
		fs.file.Header.HeaderSize = want
	}
	return nil
}

// adoptLegacy populates fs from a legacy-decoded snapshot; caller
// flushes afterwards to materialise the current format.
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

// scanExpungesSince reads every TxTypeExpungeGUID record in the .log
// file and returns the UIDs whose embedded modseq is strictly greater
// than sinceModSeq.
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
		// Treat header errors as an empty log.
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
			break // torn write; subsequent records are unrecoverable
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

// scanExpungedGUIDsSince is scanExpungesSince reading the other half of the
// same record. The expunge carries the message GUID beside its UID, which is
// what a protocol identifying messages by GUID needs: the message is gone, so
// its identity cannot be looked up anywhere else afterwards (RFC 8621 destroyed
// ids, #1216).
func scanExpungedGUIDsSince(indexPath string, sinceModSeq uint64) ([][16]byte, error) {
	logPath := indexPath + ".log"
	f, err := os.Open(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fileindex/log scan: open: %w", err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := mailindex.DecodeLogHeader(f); err != nil {
		return nil, nil //nolint:nilerr
	}
	var out [][16]byte
	hdrBuf := make([]byte, 8)
	for {
		if _, err := io.ReadFull(f, hdrBuf); err != nil {
			break
		}
		txHdr, err := mailindex.DecodeTxHeader(hdrBuf)
		if err != nil {
			break
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
			// The 20-byte form carries no modseq, so it cannot be placed in
			// time; skipping it is what the UID scan does with the same record.
			continue
		}
		if binary.LittleEndian.Uint64(payload[20:]) <= sinceModSeq {
			continue
		}
		var guid [16]byte
		copy(guid[:], payload[4:20])
		out = append(out, guid)
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

// keywordLogRecords journals a keyword change the way the format has always
// meant it to be journalled: the NAME travels inside the record, so a replay
// learns both the bit and what it stands for, and never has to consult a
// registry the log did not carry. That is why growing the registry is not a
// separate case here -- a name it has not seen simply gets the next bit.
//
// An emptied set is one RESET rather than N removals; the format has the
// record and the common "clear every label" store is one write instead of a
// list that grows with the mailbox's vocabulary.
func keywordLogRecords(uid uint32, have, want []string) [][]byte {
	added := subtractStrings(want, have)
	removed := subtractStrings(have, want)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	if len(want) == 0 {
		return [][]byte{encLogRec(mailindex.TxTypeKeywordReset, 0,
			mailindex.EncodeTxKeywordResetPayload([]mailindex.TxKeywordReset{{UID1: uid, UID2: uid}}))}
	}
	out := make([][]byte, 0, len(added)+len(removed))
	for _, set := range []struct {
		names  []string
		modify uint8
	}{{added, mailindex.TxKeywordModifyAdd}, {removed, mailindex.TxKeywordModifyRemove}} {
		for _, name := range set.names {
			out = append(out, encLogRec(mailindex.TxTypeKeywordUpdate, 0,
				mailindex.EncodeTxKeywordUpdatePayload(mailindex.TxKeywordUpdate{
					ModifyType: set.modify,
					Name:       name,
					UIDRanges:  []mailindex.TxKeywordUIDRange{{UID1: uid, UID2: uid}},
				})))
		}
	}
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

// appendMutLog writes pre-encoded tx records to the .index.log file,
// wrapped in a BOUNDARY record so the group is atomic on recovery.
// Caller must hold fs.mu. fs.logFD stays open across calls; closeFDs()
// must be called before any operation that replaces the log file.
func (fs *folderState) appendMutLog(records ...[]byte) error {
	t0 := time.Now()

	if fs.logFD == nil {
		logPath := fs.indexPath + ".log"
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("fileindex/mutlog: open: %w", err)
		}
		st, _ := f.Stat()
		if st != nil && st.Size() == 0 {
			// FileSeq carries the base's lineage, which is what makes the log
			// self-describing: a reader can tell whether this log belongs to
			// the base it is holding.
			hdr := mailindex.NewLogHeader(fs.file.Header.IndexID, fs.lineage.Lineage, uint32(time.Now().Unix()))
			if err := hdr.Encode(f); err != nil {
				_ = f.Close()
				return fmt.Errorf("fileindex/mutlog: write header: %w", err)
			}
		} else if st != nil {
			fs.logSize = st.Size()
		}
		fs.logFD = f
	}

	// Compute total size: 12-byte BOUNDARY record + all sub-records.
	subSize := 0
	for _, rec := range records {
		subSize += len(rec)
	}
	boundary := encLogRec(mailindex.TxTypeBoundary, 0,
		mailindex.EncodeTxBoundaryPayload(mailindex.TxBoundary{Size: uint32(12 + subSize)}))

	// Single write: BOUNDARY + sub-records must land atomically so a
	// concurrent applyLog cannot see a BOUNDARY whose payload is not yet on
	// disk and truncate a committed update.
	buf := make([]byte, 0, 12+subSize)
	buf = append(buf, boundary...)
	for _, rec := range records {
		buf = append(buf, rec...)
	}
	if _, err := fs.logFD.Write(buf); err != nil {
		_ = fs.logFD.Close()
		fs.logFD = nil
		return fmt.Errorf("fileindex/mutlog: write: %w", err)
	}
	fs.logSize += int64(len(buf))

	if dur := time.Since(t0); dur > 100*time.Millisecond {
		slog.Debug("fileindex: slow mutlog write", "folder", fs.folder, "dur_ms", dur.Milliseconds())
	}
	return nil
}

// applyLog reads tx records from .index.log starting at fromOffset and
// applies them to fs.file. Caller must hold fs.mu.
//
// Returns the absolute offset BOUNDARY-confirmed as fully applied, never
// an os.Stat size: only this value may advance fs.logSize. A stat could
// claim bytes this call never parsed, permanently wedging reload's fast
// path; under-reporting merely costs an idempotent re-apply.
//
// Keywords extension data is NOT updated from log records; cross-pod
// keyword visibility requires OptimizeIndex to compact the log.
func (fs *folderState) applyLog(fromOffset int64) (int64, error) {
	lg, err := openLogRead(fs.indexPath)
	if err != nil {
		return fromOffset, fmt.Errorf("fileindex/applylog: open: %w", err)
	}
	defer lg.close()
	return fs.applyLogFrom(lg, fromOffset)
}

// applyLogFrom folds in the log lg holds open, starting at fromOffset. Taking
// the reader rather than a path is the point: the caller decided where to start
// from THIS descriptor's header, so the body it reads has to be the same one.
func (fs *folderState) applyLogFrom(lg *logReader, fromOffset int64) (int64, error) {
	if lg.f == nil || !lg.ok {
		return fromOffset, nil // absent, empty or unreadable log
	}
	f := lg.f
	if lh := lg.hdr; lh.IndexID != fs.file.Header.IndexID {
		// Log belongs to a different (deleted/recreated) mailbox at this
		// path; caller flushes a fresh base + empty log.
		return fromOffset, errLogIndexIDMismatch
	}
	// The reader consumed the header when it opened, so seek explicitly rather
	// than inheriting whatever position the last user of this descriptor left.
	// fromOffset itself is not rewritten: zero means "full replay" further
	// down, where it gates the torn-tail truncate.
	start := int64(mailindex.LogHeaderSize)
	if fromOffset > start {
		start = fromOffset
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fromOffset, fmt.Errorf("fileindex/applylog: seek: %w", err)
	}

	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fromOffset, fmt.Errorf("fileindex/applylog: record layout: %w", err)
	}

	var maxModseq uint64
	le := binary.LittleEndian
	hdrBuf := make([]byte, 8)
	appendedMsgs := false

	// filePos and committedEnd are absolute file offsets. committedEnd
	// tracks the offset after the last complete BOUNDARY seen during this
	// call, so a partial/torn trailing group is excluded from the confirmed
	// return value on incremental reads too.
	filePos := fromOffset
	if fromOffset == 0 {
		filePos = int64(mailindex.LogHeaderSize)
	}
	// Bytes covered by a previous confirmed pass or the header are trusted
	// as a baseline even if this call confirms nothing new.
	committedEnd := filePos

	for {
		recStart := filePos
		n, err := io.ReadFull(f, hdrBuf)
		filePos += int64(n)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		} else if err != nil {
			return committedEnd, fmt.Errorf("fileindex/applylog: read hdr: %w", err)
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
		n, err = io.ReadFull(f, payload)
		filePos += int64(n)
		if err != nil {
			break
		}

		kind := txHdr.Type.Kind()

		if kind == mailindex.TxTypeBoundary {
			if len(payload) >= 4 {
				committedEnd = recStart + int64(le.Uint32(payload))
			}
			continue
		}

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
				// The 28-byte form carries the expunge modseq at offset 20. Feed
				// it into maxModseq so a cross-process reader advances its header
				// HighestModSeq on an expunge — otherwise a sibling process's
				// NextModSeq reuses the expunge's modseq for the next delivery,
				// breaking modseq monotonicity and the poll-based new-mail refresh.
				if stride == 28 {
					if ms := le.Uint64(payload[i+20:]); ms > maxModseq {
						maxModseq = ms
					}
				}
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
					case 20:
						fs.file.Header.Flags = mailindex.HeaderFlag(v)
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

		case kind == mailindex.TxTypeKeywordUpdate:
			rec, ok := mailindex.DecodeTxKeywordUpdatePayload(payload)
			if !ok {
				// The framing already passed, so this is not a torn tail: it is
				// a whole record too short to hold its own name. Skipping it
				// would be the #1314 class one floor down.
				return committedEnd, fmt.Errorf("fileindex/applylog: malformed keyword record (type %#x) at offset %d", uint32(kind), recStart)
			}
			// The name arrived with the record, so the registry is grown from
			// the log itself: no adapter, and no separate case for a keyword
			// this reader has never seen.
			bits, reg, kwErr := keywordsBitmaskFor(fs.keywords, []string{rec.Name})
			if kwErr != nil {
				// The 32-bit ceiling. Swallowing it here would drop the word
				// in silence, which is the defect this whole change is about.
				return committedEnd, fmt.Errorf("fileindex/applylog: keyword %q: %w", rec.Name, kwErr)
			}
			fs.keywords = reg
			if regErr := fs.persistKeywordRegistry(); regErr != nil {
				return committedEnd, fmt.Errorf("fileindex/applylog: keyword registry: %w", regErr)
			}
			for _, r := range rec.UIDRanges {
				for _, mr := range fs.file.Records {
					if mr.UID < r.UID1 || mr.UID > r.UID2 {
						continue
					}
					cur := decodeKeywordsRec(mr.Ext[extNameKeywords])
					if rec.ModifyType == mailindex.TxKeywordModifyRemove {
						cur &^= bits
					} else {
						cur |= bits
					}
					if mr.Ext == nil {
						mr.Ext = make(map[string][]byte)
					}
					mr.Ext[extNameKeywords] = encodeKeywordsRec(cur)
				}
			}

		case kind == mailindex.TxTypeKeywordReset:
			for _, r := range mailindex.DecodeTxKeywordResetPayload(payload) {
				for _, mr := range fs.file.Records {
					if mr.UID < r.UID1 || mr.UID > r.UID2 {
						continue
					}
					if mr.Ext == nil {
						mr.Ext = make(map[string][]byte)
					}
					mr.Ext[extNameKeywords] = encodeKeywordsRec(0)
				}
			}

		case kind == mailindex.TxTypeExpunge || kind == mailindex.TxTypeExpungeGUID:
			// A known type judged corrupt, not an unknown one: an expunge
			// without its corruption-defence bit is ignored by the format's
			// own rule, and saying so here keeps it out of the refusal below.

		default:
			// Proceeding past a record we cannot read reports a fully replayed
			// tail and a state missing whatever it said -- silence in the shape
			// of an answer (#1314). Refuse instead: an open that fails names
			// the version skew, a mailbox quietly missing a keyword does not.
			return committedEnd, fmt.Errorf("fileindex/applylog: unknown transaction type %#x at offset %d", uint32(kind), recStart)
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

	// Recount message counters from actual records so that any drift introduced
	// by a corrupted TxTypeHeaderUpdate (e.g. from a stale SaveFolder flush) is
	// corrected in memory immediately after log replay, not only at the next flush.
	fs.file.Header.MessagesCount = uint32(len(fs.file.Records))
	fs.file.Header.SeenMessagesCount = 0
	fs.file.Header.DeletedMessagesCount = 0
	for _, rec := range fs.file.Records {
		if rec.Flags&mailindex.FlagSeen != 0 {
			fs.file.Header.SeenMessagesCount++
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			fs.file.Header.DeletedMessagesCount++
		}
	}

	// Truncate any partial tail after the last complete BOUNDARY. Only on full
	// replay (fromOffset==0); incremental appends are always complete.
	//
	// Compares against filePos — how far THIS read pass actually got, including
	// any trailing bytes it tried and failed to parse as a complete record —
	// never a fresh os.Stat. This function commonly runs unlocked (readBase's
	// fast "folder already exists" path, taken on every new connection opening
	// an established folder). A concurrent writer's appendMutLog can complete
	// its own fully-valid, atomic write in the gap between this read loop
	// hitting EOF and a separate later stat; that stat would then see the
	// writer's legitimate growth as "beyond what we read" and truncate it away
	// — silently destroying another process's already-committed record. Using
	// filePos instead means the truncate decision is a pure function of bytes
	// THIS call actually read and could not parse, so it can never chop off
	// data written after this pass finished reading.
	if fromOffset == 0 && committedEnd > 0 && filePos > committedEnd {
		logPath := fs.indexPath + ".log"
		slog.Debug("fileindex: truncating partial log tail",
			"folder", fs.folder, "read_size", filePos, "truncate_to", committedEnd)
		_ = os.Truncate(logPath, committedEnd)
	}
	// Return committedEnd, not filePos: on an incremental read (fromOffset>0)
	// a torn/partial trailing group is never truncated (a legitimate writer
	// may still be mid-append at that exact position — see the no-truncate
	// rationale above), but it must also not be reported as confirmed. The
	// next reload() naturally retries from this same conservative point.
	return committedEnd, nil
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
	if err := fs.appendName(rec.UID, fs.filenames[rec.UID], fs.sizes[rec.UID]); err != nil {
		return fmt.Errorf("fileindex/append: names: %w", err)
	}
	// Emit a TxModseqUpdate alongside the append so a cross-process reader that
	// picks up this append via the log advances its header HighestModSeq — applyLog
	// only raises the header modseq from TxModseqUpdate records, and the append's
	// own record-level modseq does not feed it. Without this, a delivered message
	// leaves HighestModSeq stale for other sessions (breaks CONDSTORE HIGHESTMODSEQ
	// and the IMAP poll-based refresh that adds new UIDs to a selected session).
	modseq := decodeModseqRec(rec.Ext[extNameModSeq])
	return fs.appendMutLog(
		encLogRec(mailindex.TxTypeAppend, 0, appendPayload),
		encLogRec(mailindex.TxTypeModseqUpdate, 0, mailindex.EncodeTxModseqUpdatePayload([]mailindex.TxModseqUpdate{{
			UID: rec.UID, ModSeqLow32: uint32(modseq), ModSeqHigh32: uint32(modseq >> 32),
		}})),
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
// truncateLogLineage replaces the log with an empty one carrying lineage, so
// the fresh log announces which base it belongs to. A zero lineage is what a
// base written before the extension gives, and it reads as "proves nothing".
func truncateLogLineage(indexPath string, indexID, lineage uint32) error {
	logPath := indexPath + ".log"
	tmp := logPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileindex/log truncate: open: %w", err)
	}
	hdr := mailindex.NewLogHeader(indexID, lineage, uint32(time.Now().Unix()))
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

// SetCacheOffsets stamps cache-file offsets for the given UIDs (#1030).
// Batched by design: the writer parses a FETCH's worth of messages and
// stamps them in one flush. Unlike SetGUIDs an offset MAY be overwritten --
// a new record appended to a message's chain has a new offset -- but only
// with a non-zero value; zeroing is the expunge/reconcile paths' job. An
// index predating the extension gains it here, lazily, like guid does.
func (u *userIndex) SetCacheOffsets(folderID uint64, offsets map[uint32]uint32) error {
	if len(offsets) == 0 {
		return nil
	}
	return u.withFolder(folderID, func(fs *folderState) error {
		if findExt(fs.file.Extensions, extNameCache) == nil {
			if err := fs.file.AddRecordExtension(extNameCache, nil,
				cacheRecSize, 4, fs.file.Header.UIDValidity); err != nil {
				return fmt.Errorf("fileindex: add cache extension: %w", err)
			}
		}
		for _, rec := range fs.file.Records {
			off, ok := offsets[rec.UID]
			if !ok || off == 0 {
				continue
			}
			if rec.Ext == nil {
				rec.Ext = make(map[string][]byte, 1)
			}
			rec.Ext[extNameCache] = encodeCacheRec(off)
		}
		return fs.flush(true)
	})
}

// CachePairIdentity returns what the cache layer needs to open or create the
// paired yarilo.index.cache: the index identity and the cache extension's
// reset_id (== the file_seq a valid cache file must carry). ok is false when
// the folder's index predates the extension and no offset was ever stamped.
func (u *userIndex) CachePairIdentity(folderID uint64) (indexID, resetID uint32, ok bool, err error) {
	err = u.withFolderRO(folderID, func(fs *folderState) error {
		indexID = fs.file.Header.IndexID
		if ext := findExt(fs.file.Extensions, extNameCache); ext != nil {
			resetID = ext.ResetID
			ok = true
		}
		return nil
	})
	return indexID, resetID, ok, err
}

// CachePath is where the folder's yarilo.index.cache lives: beside
// yarilo.index in the folder's index directory.
func (u *userIndex) CachePath(folderID uint64) (string, error) {
	var path string
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		path = filepath.Join(fs.indexDir, mailindex.CacheFileName)
		return nil
	})
	return path, err
}

// PurgeCache rewrites the folder's cache as a new generation holding only
// what live messages point at (#1030). Returns records carried and bytes
// reclaimed.
//
// Write the new file, rename it over, then move the extension's reset_id --
// in that order, so a crash between steps leaves a file_seq mismatch, which
// readers already treat as "no cache, rebuild". No directory fsync: unlike an
// FTS shard, a cache generation back from the dead carries the old file_seq
// and invalidates itself.
func (u *userIndex) PurgeCache(folderID uint64) (carried int, reclaimed int64, err error) {
	err = u.withFolder(folderID, func(fs *folderState) error {
		ext := findExt(fs.file.Extensions, extNameCache)
		if ext == nil {
			return nil // nothing was ever cached
		}
		path := filepath.Join(fs.indexDir, mailindex.CacheFileName)
		before, serr := os.Stat(path)
		if serr != nil {
			return nil // no cache file
		}
		old, oerr := mailindex.OpenCache(path, fs.file.Header.IndexID, ext.ResetID)
		if oerr != nil {
			// Already invalid: drop it -- and enter a new generation, or the
			// stamps still in the index would apply to whatever is written
			// at those offsets next.
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				return fmt.Errorf("fileindex: purge cache remove: %w", rerr)
			}
			reclaimed = before.Size()
			if _, berr := abandonCacheGeneration(fs); berr != nil {
				return berr
			}
			return nil
		}

		live := make(map[uint32]uint32, len(fs.file.Records))
		for _, rec := range fs.file.Records {
			if off := decodeCacheRec(rec.Ext[extNameCache]); off != 0 {
				live[rec.UID] = off
			}
		}
		newSeq := newCacheGeneration(ext.ResetID)
		tmp := path + ".purge"
		_ = os.Remove(tmp)
		moved, perr := old.PurgeInto(tmp, newSeq, live)
		old.Close()
		if perr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("fileindex: purge cache: %w", perr)
		}
		after, aerr := os.Stat(tmp)
		if aerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("fileindex: purge cache stat: %w", aerr)
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("fileindex: purge cache rename: %w", rerr)
		}
		for _, rec := range fs.file.Records {
			off, ok := moved[rec.UID]
			if !ok {
				delete(rec.Ext, extNameCache)
				continue
			}
			if rec.Ext == nil {
				rec.Ext = make(map[string][]byte, 1)
			}
			rec.Ext[extNameCache] = encodeCacheRec(off)
		}
		ext.ResetID = newSeq
		carried = len(moved)
		reclaimed = before.Size() - after.Size()
		return fs.flush(true)
	})
	return carried, reclaimed, err
}

// newCacheGeneration returns a file_seq no live stamp can belong to. Seeded
// from the clock, as the reference seeds its first one: a counter would
// repeat after adoptLegacy, which reapplies defaultExtensions and drops every
// reset_id back to UIDValidity. Never goes backwards -- a clock that does
// would otherwise hand back a generation already used.
func newCacheGeneration(prev uint32) uint32 {
	now := uint32(time.Now().Unix())
	if now <= prev {
		return prev + 1
	}
	return now
}

// abandonCacheGeneration enters a new generation and drops every stamp
// pointing into the old one. A generation may only be left by entering the
// next: an offset kept across a file that was removed and recreated under the
// SAME file_seq stays "valid" by all four levels, and the first append to
// reuse that offset answers one message's FETCH with another's record.
func abandonCacheGeneration(fs *folderState) (uint32, error) {
	ext := findExt(fs.file.Extensions, extNameCache)
	if ext == nil {
		return 0, nil
	}
	ext.ResetID = newCacheGeneration(ext.ResetID)
	for _, rec := range fs.file.Records {
		delete(rec.Ext, extNameCache)
	}
	return ext.ResetID, fs.flush(true)
}

// BumpCacheGeneration abandons the current cache generation and returns the
// new file_seq, for callers that had to discard the file (#1184).
func (u *userIndex) BumpCacheGeneration(folderID uint64) (uint32, error) {
	var seq uint32
	err := u.withFolder(folderID, func(fs *folderState) error {
		var berr error
		seq, berr = abandonCacheGeneration(fs)
		return berr
	})
	return seq, err
}

// EnsureCacheExtension adds the cache extension to an index written before it
// existed, and returns the pair identity. Folders created since carry it from
// defaultExtensions; without this an older folder could never gain one, since
// the only other add sits behind a stamping write that needs the extension to
// be reachable at all (#1184).
func (u *userIndex) EnsureCacheExtension(folderID uint64) (indexID, resetID uint32, err error) {
	err = u.withFolder(folderID, func(fs *folderState) error {
		if findExt(fs.file.Extensions, extNameCache) == nil {
			// From the clock, not UIDValidity: a file left at this path by an
			// earlier life must not match the generation we are creating.
			if aerr := fs.file.AddRecordExtension(extNameCache, nil,
				cacheRecSize, 4, newCacheGeneration(0)); aerr != nil {
				return fmt.Errorf("fileindex: add cache extension: %w", aerr)
			}
			if ferr := fs.flush(true); ferr != nil {
				return ferr
			}
		}
		indexID = fs.file.Header.IndexID
		if ext := findExt(fs.file.Extensions, extNameCache); ext != nil {
			resetID = ext.ResetID
		}
		return nil
	})
	return indexID, resetID, err
}

// subtractStrings returns a without any member of b.
func subtractStrings(a, b []string) []string {
	drop := make(map[string]bool, len(b))
	for _, s := range b {
		drop[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if !drop[s] {
			out = append(out, s)
		}
	}
	return out
}

// unionStrings returns a ∪ b with the order of a preserved, then whatever b
// adds. Used where a flag or keyword write must keep what the record already
// carries.
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// stampExpungeFloorLocked records how far back this folder's expunge history
// reaches, and must be called before any path that drops the log.
//
// Expunge records live only in the transaction log. Folding the log into the
// base and truncating it takes them with it, and Vanished(since) then returns
// nothing for the window it can no longer see -- which is indistinguishable
// from "nothing was expunged". A reader diffing states would tell a client it
// is up to date while listing messages that are gone.
//
// So the fold writes down the modseq it folded at. A caller asking about a
// point before the floor is told the history is unavailable, and refetches,
// instead of being handed a confident empty answer.
//
// The floor is deliberately conservative: a `since` between the last expunge
// and the fold point is refused too, although nothing was actually removed in
// that window. That direction is safe -- it costs one extra full resync and
// can never produce a phantom message. Do not "optimise" it into a precise
// last-expunge marker without solving what that marker means for a log that no
// longer exists.
func (fs *folderState) stampExpungeFloorLocked() error {
	modseq, err := fs.highestModSeq()
	if err != nil {
		return err
	}
	ext := findExt(fs.file.Extensions, extNameExpungeFloor)
	if ext == nil {
		fs.file.Extensions = append(fs.file.Extensions, mailindex.Extension{
			Name:        extNameExpungeFloor,
			HdrSize:     expungeFloorSize,
			HdrData:     encodeExpungeFloor(modseq),
			RecordSize:  0,
			RecordAlign: 8,
			ResetID:     fs.file.Header.UIDValidity,
		})
		return fs.syncHeaderSizeLocked()
	}
	if decodeExpungeFloor(ext.HdrData) >= modseq {
		// Never lower it: a floor that moves backwards would promise history
		// the log no longer has.
		return nil
	}
	ext.HdrData = encodeExpungeFloor(modseq)
	ext.HdrSize = expungeFloorSize
	return fs.syncHeaderSizeLocked()
}

// expungeFloorLocked reads the folder's floor. Zero means nothing has ever been
// folded away, so the log is the whole history.
func (fs *folderState) expungeFloorLocked() uint64 {
	ext := findExt(fs.file.Extensions, extNameExpungeFloor)
	if ext == nil {
		return 0
	}
	return decodeExpungeFloor(ext.HdrData)
}
