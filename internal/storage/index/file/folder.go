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

// OpenFolder opens or creates the per-folder index; uidValidity applies only to
// a fresh folder. A legacy .index is migrated on first open, leaving a backup.
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
			// Re-opening what this index holds: the lock-free read FolderVSize
			// makes, with the locked one as its own fallback (#1639).
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

	// indexDir depends on u.driver: a mismatch computes a different path for
	// one folder and registers a disconnected folderState.
	slog.Debug("fileindex: openfolder first-open, computing layout",
		"trace_id", traceID, "folder", folder, "driver", u.driver, "index_dir", indexDir)

	if u.b.noCreate {
		// Before any mkdir: a no-create open must leave the filesystem as it
		// found it, or a mis-resolved path still gets a directory chain.
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
		// Theirs, under a name that was ours once. A dbox folder is converted
		// from it and keeps it until then; maildir is served from its own files
		// and never reads it, so there it is dead weight (#1593).
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
// first open after the upgrade -- without it a read-only folder never flushes
// and stays unable to prove freshness forever, falling back to the locked path
// on every read. Exactly what the first measurement showed: adopt zero,
// acquisitions unchanged (#1229). One flush per folder, under the exclusive
// lock, announced once so the one-time cost does not read as a mystery.
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
		// Flush only: truncating here would lose a concurrent writer's committed
		// entries. The flush folds the log in and records how far it reached.
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
			// No floor stamp: this log is a header and nothing else, so raising
			// the floor would cost a resync for a truncate that lost nothing.
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

// loadOrInit populates fs.file from the existing .index, a legacy migration or
// a fresh file. The initial stat is unlocked: only ErrNotExist needs the lock.
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
		// An unreadable format is the state of the data, not a fault here, and
		// it stops at one folder -- named so the layers above can say which.
		return asCorrupt(fs.folder, err)
	}
	return nil
}

// loadOrInitMissing handles ErrNotExist under the lock: two openers can both see
// it unlocked, and without the re-stat the loser's createFresh resets NextUID.
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
			// have written it, and a fresh empty index would hide it (#1524).
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
		// Legacy migration writes the index, so re-detect under the folder lock:
		// a racer that already migrated wins and we load its file.
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

// readBase opens the base into fs.file, records its mtime and replays the log,
// resetting a mismatched-IndexID one under the folder lock.
func (u *userIndex) readBase(fs *folderState) error {
	mf, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/openfolder: open: %w", err)
	}
	fs.file = mf
	// Every path that loads a base learns its pairing here, so the lineage
	// question is answered by the file, not by which function loaded it.
	fs.lineage = readLineage(mf)
	if st, stErr := os.Stat(fs.indexPath); stErr == nil {
		fs.baseMod = st.ModTime()
		fs.baseIdent = st
	}
	// fs.logSize comes from applyLog's confirmed offset, never a stat: a
	// post-call stat over-reports an append it never parsed, wedging reload.
	if _, logErr := os.Stat(fs.indexPath + ".log"); logErr == nil {
		confirmedEnd, applyErr := fs.applyLog(0)
		if errors.Is(applyErr, errLogIndexIDMismatch) {
			// Log belongs to a deleted/recreated mailbox; reset it under the
			// distributed lock so concurrent writers don't race the truncate.
			if lockErr := u.withFolderLock(fs, func() error {
				slog.Warn("fileindex: discarding log with mismatched IndexID on open",
					"folder", fs.folder)
				fs.closeFDs()
				// A zero lineage is intended, not a gap: a base may predate the
				// extension, and an unpaired log sends readers back to the lock.
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
		// The UIDVALIDITY repair writes the index, so re-read under the lock:
		// a racer that already repaired it wins.
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

// refreshExtStateFromDisk re-reads the base's extension HEADERS, never its
// records: for the path keeping the records it has but needing the headers.
func (fs *folderState) refreshExtStateFromDisk() error {
	exts, err := peekExtHeaders(fs.indexPath)
	if err != nil {
		return fmt.Errorf("fileindex/refresh: peek extension headers: %w", err)
	}
	if len(exts) == 0 {
		return nil
	}
	// The typed copies come from the fresh headers; fs.file keeps its own
	// list, describing the layout its in-memory records were decoded with.
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

// recalcVsizeLocked recomputes the aggregate from the per-record vsize
// extension, falling back to physical size for records predating it.
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

// ensureVsizeLocked recomputes the aggregate only when the O(1) validity check
// says it is stale, so a quota read does not rescan every message. Holds fs.mu.
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
	// Backfill for base indexes predating hdr-vsize. AddHeaderExtension also
	// fixes Header.HeaderSize, which Recreate rejects on mismatch.
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

// bumpModSeqHeader increments highest_modseq and returns the new value; the
// caller flushes.
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

// advanceModSeqAtLeast raises highest_modseq to target, for a caller that
// pre-allocated one: the header must reflect it without bumping past it.
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
	// flush persists Header.NextUID as ground truth and discards the log; name
	// the caller so a NextUID regression traces to the flush that wrote it.
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
	// Mint the lineage and record what this base absorbs before building it: a
	// crash before the truncation leaves a base that knows what it contains.
	prev := readLineage(fs.file)
	// Read from the log, not our own previous lineage: a pre-extension log
	// carries a constant, and assuming ours pairs a base with a foreign log.
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
	// Lineages start above the pre-extension constant, so a stamped base cannot
	// claim such a log as its own and replay what it already absorbed.
	if next.Lineage < legacyLogLineage+1 {
		next.Lineage = legacyLogLineage + 1
	}
	if err := setLineage(fs.file, next); err != nil {
		return fmt.Errorf("fileindex/flush: lineage: %w", err)
	}
	// One truth for the header size, recomputed here: a path that grew an
	// extension without it produced a base Recreate refuses (#1285).
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
	// Durability off by default: the rename is atomic and a lost tail re-derives
	// from the log. Conversion removes the only other copy right after (#1524).
	ri.Fsync = fs.fsyncOnFlush
	if _, err := mailindex.Recreate(ri); err != nil {
		// A rejected base stays unwritable until someone sees WHY: the error
		// names a size disagreement, not which extension carries it (#1285).
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

// withFolder locks folderID's state, reloads and runs fn against the freshest
// committed state. A missing file is swallowed so the caller can createFresh.
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
	// One wrapper for every path out of the read (#1344), naming the folder so
	// layers above answer per folder rather than per account.
	return asCorrupt(fs.folder, fs.reloadLocked())
}

// asCorrupt names the folder on an error meaning the disk holds what this
// version does not read -- the data's state, and it stops at one folder.
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

	// Identity, since coarse mtime resolution hides a same-tick replace. True
	// only when both stats are known and differ; unknown falls back to mtime.
	baseReplaced := fs.baseIdent != nil && baseStat != nil && !os.SameFile(fs.baseIdent, baseStat)

	// Fast path, never taken when the log was replaced: a concurrent compaction
	// rewrote the base too, and its new mtime may coincide with the cached one.
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
		// A rewritten base often holds exactly what this handle holds -- a
		// compaction folding in the applied log. The digest proves it: several
		// paths fold the same log, so offsets agreeing is not enough (#1228).
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
		// Where to resume in the log the base did not fully absorb: without the
		// pairing this restarts from zero, relying on idempotence nobody declared.
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

// applyLogTail folds in what the log gained past this handle -- both reload and
// adopt end here, since a new base's writer may already have appended to it.
func (fs *folderState) applyLogTail(lg *logReader) error {
	if lg.size > fs.logSize {
		// fs.logSize comes from applyLog's confirmed return, not the pre-call
		// stat: an append mid-read is re-applied by the next reload.
		if confirmedEnd, err := fs.applyLogFrom(lg, fs.logSize); errors.Is(err, errLogIndexIDMismatch) {
			// Stale log from a previous mailbox at this path: flush the
			// current base and reset the log.
			slog.Warn("fileindex: discarding log with mismatched IndexID, re-flushing base",
				"folder", fs.folder)
			// Conservative: those expunges were a different mailbox's, and
			// raising the floor costs a resync where leaving it costs a phantom.
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

// SaveFolder persists header-level mutations from f. Record-state changes are
// ignored; callers use AppendMessage, UpdateFlags or ExpungeMessage.
func (u *userIndex) SaveFolder(f *mailbox.Folder) error {
	return u.withFolder(f.ID, func(fs *folderState) error {
		return fs.flush(false)
	})
}

// AdoptUIDSpace sets UIDVALIDITY and next UID from a store that records them,
// refusing a folder that holds messages -- changing the UID space of a mailbox
// a session may have seen is what UIDVALIDITY exists to prevent.
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

// AppendMessage records m as a new on-disk record; m.UID must already be
// assigned, by AllocateUID or by an external authority.
func (u *userIndex) AppendMessage(folderID uint64, m *mailbox.MessageMeta) error {
	if err := u.withFolder(folderID, func(fs *folderState) error {
		// next_uid_before exposes a UID-reuse race: a commit below it means the
		// counter advanced past this UID since AllocateUID ran.
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

// FolderVSize returns the aggregate the quota backend sums, from the hdr-vsize
// cache. A read, taken as one: through the write path it cost an exclusive lock
// per folder before every save (#1634).
func (u *userIndex) FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error) {
	err = u.withFolderROUnlocked(folderID, func(fs *folderState) error {
		bytes = fs.vsize.Vsize
		messages = fs.vsize.MessageCount
		return nil
	})
	return bytes, messages, err
}

// RecomputeVSize rebuilds the hdr-vsize aggregate from the per-record extension
// and persists it -- the admin path for a corrupt one; normal reads self-heal.
func (u *userIndex) RecomputeVSize(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		fs.recalcVsizeLocked()
		fs.persistVsizeLocked()
		return fs.flush(false)
	})
}

// GUIDBackfillNeeded reads the guid extension header; an index predating it
// decodes as pending, which is exactly the set still carrying zero GUIDs.
func (u *userIndex) GUIDBackfillNeeded(folderID uint64) (bool, error) {
	var need bool
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		ext := findExt(fs.file.Extensions, extNameGUID)
		need = ext == nil || decodeGUIDHdr(ext.HdrData) != guidStateComplete
		return nil
	})
	return need, err
}

// SetGUIDs stamps GUIDs onto records that have none and flips the header to
// complete, leaving existing ones alone -- so an interrupted pass resumes.
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

// AllocateUID reserves and persists the next UID for the caller to pass to Save
// and AppendMessage; a crash between them burns it, and the rebuild reconciles.
// One cross-process lock covers the read-modify-write window.
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

// appendLocked is the in-memory half of AppendMessage; caller holds the folder
// lock. A non-zero m.ModSeq is pre-allocated and recorded as-is; zero bumps the
// counter and writes the new value back into m.
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
	// The registry grew: persist the extension headers so a cross-pod reader
	// can decode the bitmasks. Rare -- first use of each name only.
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

// AddFlags unions flags and keywords against the record as the lock finds it,
// unlike UpdateFlags's absolute list, which overwrites changes since an earlier
// read -- the implicit \Seen of a non-PEEK FETCH being that caller (#1250).
func (u *userIndex) AddFlags(folderID uint64, uid uint32, flags, keywords []string) error {
	return u.writeFlags(folderID, uid, flags, keywords, flagsAdd)
}

// RemoveFlags clears flags and keywords, leaving the rest as the lock finds
// them: AddFlags's counterpart, since UpdateFlags needs a whole set read earlier.
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
		// The record's own keywords under the lock: Add/Remove fold into them,
		// so one set since the caller's read is not dropped.
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

// beforeAppendName runs before each name is written. Test seam: the append
// clock must be proven to span the writes, since a fast disk reports zero.
var beforeAppendName func()

// UpdateFilenames records a command's new names under one acquisition: the
// single form locks per message, costing a 41-name STORE 17.7s of 18.1s (#1646).
func (u *userIndex) UpdateFilenames(folderID uint64, names map[uint32]string) error {
	if len(names) == 0 {
		return nil
	}
	u.mu.Lock()
	fs, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("fileindex: folder %d not open", folderID)
	}

	// Timed in three: one acquisition, and a single name still cost 4s where the
	// mean was 14ms. The wait, the freshness check and the appends are separate
	// floors, and only one has a known cure (#1650).
	whole := time.Now()
	var lockMS, reloadMS, appendMS int64
	err := u.withFolderLock(fs, func() error {
		lockMS = time.Since(whole).Milliseconds()
		reloadStart := time.Now()
		if rerr := fs.reload(); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		reloadMS = time.Since(reloadStart).Milliseconds()
		appendStart := time.Now()
		defer func() { appendMS = time.Since(appendStart).Milliseconds() }()
		for uid, filename := range names {
			if beforeAppendName != nil {
				beforeAppendName()
			}
			// A uid the folder no longer carries is skipped, not refused: it was
			// expunged between the rename and here, and the rest must land.
			if cur, have := fs.filenames[uid]; !have || cur == filename {
				continue
			}
			fs.filenames[uid] = filename
			if aerr := fs.appendName(uid, filename, fs.sizes[uid]); aerr != nil {
				return aerr
			}
		}
		return nil
	})
	slog.Debug("fileindex: names timing",
		"user", u.username, "folder", fs.folder, "names", len(names),
		"lock_ms", lockMS, "reload_ms", reloadMS, "append_ms", appendMS,
		"total_ms", time.Since(whole).Milliseconds())
	return err
}

// UpdateFilename repoints a UID's stored filename, which lives only in the
// .names sidecar; last write wins on reload, and an unknown uid is a no-op.
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

// UpdateFlagsMulti replaces a batch's flags in one lock/reload/flush cycle,
// bumping each UID's modseq so CONDSTORE can pinpoint what changed.
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
			// Add/Remove name only what changes, so the set is resolved here
			// against the held record -- the caller's would be as old as its read.
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

// ExpungeMessage writes a TxTypeExpungeGUID log entry and drops the in-memory
// record; Vanished reads those entries later to satisfy QRESYNC.
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
			// Without the per-record vsize extension, fall back to physical
			// size as recalcVsizeLocked does, or the aggregate goes stale.
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
		// The MESSAGE's GUID, the only place identity survives the record. The
		// mailbox GUID here once gave every expunge the same value -- invisible
		// to QRESYNC, unusable by anything addressing by id (#1216).
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

// GetMessagesUnlocked answers without the cross-process lock where the files
// prove their own consistency: for readers answering a client and deciding
// nothing. A caller driving a write or delete must use GetMessages (#1249).
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

// Vanished returns every UID expunged with modseq above sinceModSeq, driving
// the QRESYNC VANISHED response (RFC 7162).
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

// KeywordsUnlocked is Keywords for SELECT: a keyword declared a moment later
// appears on the next command, the staleness the protocol accepts (#1249).
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

// ResetFolder replaces every record with the supplied set, preserving
// UIDValidity, folder GUID and indexID; NextUID is set past max(records.UID).
// Returns the dropped UIDs so the caller can invalidate their FTS documents.
// A surviving record keeps its own ModSeq, highest_modseq advances to the max
// carried in, and a record with none is stamped fresh -- a rebuild changing
// nothing leaves the header untouched, with nothing to signal QRESYNC.
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

// SetAltTier sets or clears FlagBackend on the named records, after AltMove,
// so Fetch skips the primary open for cold-tier messages.
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

// OptimizeIndex folds pending log records into the base and truncates it, so
// Vanished(since) is then empty below the current highest.
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

// VanishedGUIDs is Vanished by message identity. complete is false when a
// record cannot be named: an expunge predating the field holds the MAILBOX's
// GUID, dropped rather than handed out as an id naming no message.
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

// FolderStamp stats the folder's two files without opening it, so a cached
// marker costs two stats instead of a read and a replay. A missing file is -1,
// keeping "not there" and "empty" apart.
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

// ExpungeFloor reports the modseq below which "what was expunged since" can no
// longer be answered; zero means the log still holds the whole history. A
// caller below the floor must degrade -- a fresh JMAP listing, an empty
// VANISHED (EARLIER) -- not read an empty answer as "nothing was deleted"
// (#1216).
func (u *userIndex) ExpungeFloor(folderID uint64) (uint64, error) {
	var floor uint64
	err := u.withFolder(folderID, func(fs *folderState) error {
		floor = fs.expungeFloorLocked()
		return nil
	})
	return floor, err
}

// JournalSizes reports the base and log sizes now; a missing log is -1, a state
// drivers reach on purpose. Measured here since the paths are this package's and
// a caller reconstructing them would drift.
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

// syncHeaderSizeLocked recomputes Header.HeaderSize the way Recreate validates
// it -- a barrier every write passes, not a rule every writer must remember.
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

// scanExpungesSince returns the UIDs from every TxTypeExpungeGUID record whose
// embedded modseq is above sinceModSeq.
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

// scanExpungedGUIDsSince reads the record's other half, the message GUID --
// the only place identity survives once the message is gone (#1216).
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

// keywordLogRecords journals a keyword change with the NAME inside the record,
// so a replay learns the bit and its meaning without an external registry -- an
// unseen name gets the next bit. An emptied set is one RESET.
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

// appendMutLog writes pre-encoded tx records wrapped in a BOUNDARY, atomic on
// recovery. Caller holds fs.mu; closeFDs() runs before the log is replaced.
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
			// FileSeq carries the base's lineage, so a reader can tell whether
			// this log belongs to the base it holds.
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

	// One write: the BOUNDARY and its sub-records must land together, or a
	// concurrent applyLog truncates a committed update.
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

// applyLog reads tx records from .index.log at fromOffset into fs.file. Caller
// must hold fs.mu. Returns the offset BOUNDARY-confirmed as fully applied,
// never an os.Stat size -- a stat could claim unparsed bytes and wedge
// reload's fast path, where under-reporting only costs an idempotent re-apply.
// Keywords are not updated from log records; cross-pod visibility needs
// OptimizeIndex.
func (fs *folderState) applyLog(fromOffset int64) (int64, error) {
	lg, err := openLogRead(fs.indexPath)
	if err != nil {
		return fromOffset, fmt.Errorf("fileindex/applylog: open: %w", err)
	}
	defer lg.close()
	return fs.applyLogFrom(lg, fromOffset)
}

// applyLogFrom folds in the log lg holds open from fromOffset. It takes the
// reader, not a path: the offset came from THIS descriptor's header.
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
	// Seek explicitly rather than inheriting the descriptor's position; zero
	// still means "full replay" below, where it gates the torn-tail truncate.
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

	// Absolute offsets; committedEnd follows the last complete BOUNDARY, so a
	// torn trailing group stays out of the confirmed return.
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
				// The 28-byte form carries the expunge modseq at offset 20; a
				// reader must advance HighestModSeq from it, or a sibling reuses
				// it for the next delivery and breaks monotonicity.
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
				// The framing passed, so this is a whole record too short to
				// hold its own name -- skipping it is #1314 one floor down.
				return committedEnd, fmt.Errorf("fileindex/applylog: malformed keyword record (type %#x) at offset %d", uint32(kind), recStart)
			}
			// The name arrived with the record, so the registry grows from the
			// log itself -- no separate case for an unseen keyword.
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
			// A known type judged corrupt, not an unknown one: the format's own
			// rule ignores an expunge without its defence bit.

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

	// Recount from actual records so drift from a corrupted header update is
	// corrected right after replay, not only at the next flush.
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

	// Truncate any partial tail after the last complete BOUNDARY, only on full
	// replay (fromOffset==0) -- incremental appends are always complete.
	//
	// Compared against filePos, how far THIS pass actually read and failed to
	// parse, never a fresh os.Stat: this runs unlocked, and a concurrent
	// writer's appendMutLog can complete a fully valid write in the gap
	// between this loop hitting EOF and a separate stat, which would then see
	// the writer's legitimate growth as "beyond what we read" and truncate it
	// away. filePos makes the decision a pure function of bytes this call
	// itself could not parse, so it can never chop off data written after.
	if fromOffset == 0 && committedEnd > 0 && filePos > committedEnd {
		logPath := fs.indexPath + ".log"
		slog.Debug("fileindex: truncating partial log tail",
			"folder", fs.folder, "read_size", filePos, "truncate_to", committedEnd)
		_ = os.Truncate(logPath, committedEnd)
	}
	// committedEnd, not filePos: an incremental read neither truncates a partial
	// trailing group nor confirms it; the next reload retries from here.
	return committedEnd, nil
}

// flushAppend persists a newly appended record and updates the names sidecar;
// rec must be the last element of fs.file.Records. Caller holds fs.mu.
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
	// Emit a TxModseqUpdate alongside the append, or a cross-process reader's
	// applyLog never advances HighestModSeq from it -- only TxModseqUpdate
	// feeds the header, not the append's own record-level modseq -- leaving it
	// stale for other sessions and breaking CONDSTORE HIGHESTMODSEQ.
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

// truncateLogLineage replaces the log with an empty one carrying lineage, so
// the fresh log announces which base it belongs to -- called by OptimizeIndex
// after a successful base rewrite, once the records are absorbed into the
// snapshot. A zero lineage is what a pre-extension base gives, and it reads as
// "proves nothing".
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

// SetCacheOffsets stamps cache offsets for the given UIDs (#1030), a FETCH's
// worth per flush. An offset may be overwritten, but only with a non-zero one.
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

// CachePairIdentity returns the index identity and the reset_id a valid cache
// must carry; ok is false when the index predates the extension.
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

// PurgeCache rewrites the folder's cache as a new generation holding only what
// live messages point at (#1030), returning records carried and bytes
// reclaimed. Write, rename, then move reset_id, in that order: a crash between
// them leaves a file_seq mismatch readers already treat as "no cache, rebuild",
// so no directory fsync is needed -- a generation back from the dead
// invalidates itself.
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
			// Already invalid: drop it and enter a new generation, or the stamps
			// still in the index apply to whatever lands at those offsets.
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

// newCacheGeneration returns a file_seq no live stamp can belong to, from the
// clock: a counter repeats after adoptLegacy resets reset_id. Never goes back.
func newCacheGeneration(prev uint32) uint32 {
	now := uint32(time.Now().Unix())
	if now <= prev {
		return prev + 1
	}
	return now
}

// abandonCacheGeneration enters a new generation and drops every stamp into the
// old one. A generation may only be left by entering the next: an offset kept
// across a file recreated under the SAME file_seq still reads as valid, and the
// first append to reuse it answers one FETCH with another message's record.
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
// existed, returning the pair identity. Without it an older folder could never
// gain one: the only other add sits behind a write that needs the extension to
// be reachable already (#1184).
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

// unionStrings returns a then whatever b adds, for a write that must keep what
// the record already carries.
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
// Expunge records live only in the log; folding it into the base takes them
// with it, and Vanished(since) then returns nothing for a window it can no
// longer see -- indistinguishable from "nothing was expunged", which would
// tell a client it is up to date while listing messages that are gone. So the
// fold writes down the modseq it folded at, and a `since` before the floor is
// told the history is unavailable rather than handed a confident empty answer.
//
// Deliberately conservative: a `since` between the last expunge and the fold
// point is refused too, though nothing was removed there -- safe, costing one
// extra resync, never a phantom message. Do not sharpen it into a precise
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
