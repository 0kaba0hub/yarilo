package mdboxmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// Map is the handle for one user's map index; every mutation routes through it so
// the in-process mutex and the cross-process lock stay coherent. Not
// goroutine-safe outside its own methods.
type Map struct {
	path     string
	username string
	owner    string
	locker   locks.Locker

	mu     sync.Mutex
	st     store
	loaded bool

	// format is the base format this deployment writes (mdbox_map_format);
	// onDisk is the format the file actually carries. They differ only until the
	// next open converts one into the other.
	format Format
	onDisk Format

	// nextMapUID is the next UID an append would assign. Tracked explicitly so
	// it can be published back to callers at allocation time, not after the next
	// base rewrite.
	nextMapUID uint32

	// highestFileID is the latest value parsed from the base header; updated on
	// every AppendBatch that adds new m.<N> files.
	highestFileID uint32

	// rebuildCount is the storage-wide-rebuild generation counter. Bumped once
	// per successful rebuild.
	rebuildCount uint32

	// createFileID / createTime stamp the current append file for the age check,
	// persisted because btime is unreliable over NFS.
	createFileID uint32
	createTime   uint64

	// indexID pairs the base with its append log; a log carrying a different id
	// belongs to an earlier map at this path and is discarded, never applied.
	indexID uint32

	// lineage names the log the base on disk is the root of; foldedLineage /
	// foldedOffset name the log that base absorbed and how far into it.
	lineage       uint32
	foldedLineage uint32
	foldedOffset  int64

	// logLineage is the lineage of the log this handle has been applying, and
	// logSize how far into it. The pair is what the handle compares against a
	// rewritten base to decide whether its records are already that base's.
	logLineage uint32

	// rotateSize is the per-m.<N> size cap the batch append path enforces. 0
	// means the package default (defaultRotateSize).
	rotateSize uint32

	// logRotate* is the log's rotation triple; logRotateSet separates "not
	// configured" from a configured 0, which disables rotation.
	logRotateSet     bool
	logRotateMinSize int64
	logRotateMaxSize int64
	logRotateMinAge  time.Duration

	// baseInfo / logSize are what this handle has applied: the stat of the base
	// the records came from, and how far into the log it has replayed.
	baseInfo os.FileInfo
	logSize  int64
	// inReload is true while a freshness check is running, so its parts are
	// timed against a whole that exists. Guarded by m.mu like the rest.
	inReload bool
}

// Format names an on-disk base-index format.
type Format string

const (
	// FormatV2 is the fixed-width base: constant-time record addressing, binary
	// search over the bytes, and the log bookkeeping that makes a freshness
	// check cheap and a crash between base write and log removal survivable.
	FormatV2 Format = "v2"
	// FormatV1 is the mailindex-backed base. Selectable so a deployment can keep
	// its maps readable by an older binary; see formatv1.go for what it gives up.
	FormatV1 Format = "v1"
)

// Option configures Map construction.
type Option func(*Map)

// WithFormat selects the base format this handle writes; one already on disk in
// the other is converted on open. An unknown format is a wiring error.
func WithFormat(f Format) Option {
	return func(m *Map) { m.format = f }
}

// WithLocker wires a yarilo-locks client. A nil Locker leaves only the
// in-process Mutex as the barrier: never safe in k8s production, unit tests
// only.
func WithLocker(l locks.Locker) Option {
	return func(m *Map) { m.locker = l }
}

// WithOwner sets the lock-owner string surfaced to yarilo-locks.
// Defaults to "<process>/<pid>/<user>" when unset.
func WithOwner(s string) Option {
	return func(m *Map) { m.owner = s }
}

// defaultRotateSize is the per-m.<N> size cap when none is configured (the
// mdbox_rotate_size default, 10 MiB).
const defaultRotateSize uint32 = 10 * 1024 * 1024

// WithRotateSize sets the per-m.<N> size cap the batch append path enforces.
// 0 selects defaultRotateSize.
func WithRotateSize(n uint32) Option {
	return func(m *Map) { m.rotateSize = n }
}

// WithLogRotation sets the log's rotation triple. A zero field takes the default;
// an explicitly configured zero minSize disables rotation, as in the file index.
func WithLogRotation(minSize, maxSize int64, minAge time.Duration) Option {
	return func(m *Map) {
		m.logRotateMinSize = minSize
		m.logRotateMaxSize = maxSize
		m.logRotateMinAge = minAge
		m.logRotateSet = true
	}
}

func (m *Map) logRotateMinSizeOrDefault() int64 {
	if !m.logRotateSet {
		return defaultLogRotateMinSize
	}
	return m.logRotateMinSize
}

func (m *Map) logRotateMaxSizeOrDefault() int64 {
	if m.logRotateMaxSize == 0 {
		return defaultLogRotateMaxSize
	}
	return m.logRotateMaxSize
}

func (m *Map) logRotateMinAgeOrDefault() time.Duration {
	if !m.logRotateSet {
		return defaultLogRotateMinAge
	}
	return m.logRotateMinAge
}

// rotateSizeOrDefault returns the configured rotate size, or the package default.
func (m *Map) rotateSizeOrDefault() uint32 {
	if m.rotateSize == 0 {
		return defaultRotateSize
	}
	return m.rotateSize
}

// Open opens or creates the per-user mdbox map at dir, migrating a legacy-named
// file in place on first open. username is the cross-process lock key.
func Open(dir, username string, opts ...Option) (*Map, error) {
	m := &Map{
		path:     filepath.Join(dir, MapIndexFileName),
		username: username,
		format:   FormatV2,
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.format == "" {
		m.format = FormatV2
	}
	if m.format != FormatV2 && m.format != FormatV1 {
		return nil, fmt.Errorf("mdboxmap/open: unknown map format %q, want %q or %q", m.format, FormatV2, FormatV1)
	}
	if m.owner == "" {
		proc := "yarilo"
		if len(os.Args) > 0 {
			proc = filepath.Base(os.Args[0])
		}
		m.owner = fmt.Sprintf("%s/%d/%s", proc, os.Getpid(), username)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mdboxmap/open: mkdir: %w", err)
	}
	start := time.Now()
	if err := m.loadOrInit(); err != nil {
		return nil, err
	}
	metricMapOpenSeconds.Observe(time.Since(start).Seconds())
	return m, nil
}

// Close releases per-handle state. Idempotent, no I/O.
func (m *Map) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st = store{}
	m.loaded = false
	return nil
}

// loadOrInit reads the base or creates one, carrying the two once-per-user
// transitions: a legacy rename and a v1 conversion. An unnamed version is refused
// -- this index decides which file a purge may unlink, so a misparse is mail loss.
func (m *Map) loadOrInit() error {
	if _, err := os.Stat(m.path); errors.Is(err, os.ErrNotExist) {
		legacy := filepath.Join(filepath.Dir(m.path), LegacyMapIndexFileName)
		if _, lerr := os.Stat(legacy); lerr == nil {
			// Only if it is ours: that name is another implementation's now,
			// and renaming theirs takes their base away before anything decided
			// to touch this store, then misreads it as ours (#1590).
			if foreign, ferr := looksForeignMapBase(legacy); ferr != nil {
				return ferr
			} else if foreign {
				return m.createFresh()
			}
			if err := os.Rename(legacy, m.path); err != nil {
				return fmt.Errorf("mdboxmap/load: migrate legacy %s: %w", legacy, err)
			}
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return fmt.Errorf("mdboxmap/load: legacy stat: %w", lerr)
		} else {
			return m.createFresh()
		}
	} else if err != nil {
		return fmt.Errorf("mdboxmap/load: stat: %w", err)
	}

	tBase := time.Now()
	err := m.readBaseLocked()
	if isForeignBase(err) {
		err = m.readBaseV1Locked()
	}
	metricMapOpenPart.WithLabelValues("base").Observe(time.Since(tBase).Seconds())
	if err != nil {
		return err
	}
	if err := m.convertToConfiguredLocked(); err != nil {
		return err
	}

	tReplay := time.Now()
	applied, rerr := m.replayFromPersistedLocked()
	metricMapOpenPart.WithLabelValues("replay").Observe(time.Since(tReplay).Seconds())
	if errors.Is(rerr, errLogIndexMismatch) {
		_ = os.Remove(m.logPath())
		applied = 0
	} else if rerr != nil {
		return rerr
	}
	m.logSize = applied
	return nil
}

// readBaseLocked reads the whole fixed-width base into memory. The records need
// no parsing: the byte area is the lookup structure.
func (m *Map) readBaseLocked() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("mdboxmap/load: read: %w", err)
	}
	h, err := decodeBaseHeader(data)
	if err != nil {
		return err
	}
	want := baseHeaderLen + int(h.RecordCount)*baseRecordLen
	if len(data) < want {
		return fmt.Errorf("mdboxmap/load: base holds %d bytes, header claims %d records", len(data), h.RecordCount)
	}
	m.st.recs = data[baseHeaderLen:want]
	m.loaded = true
	m.onDisk = FormatV2
	m.applyHeaderLocked(h)
	if st, serr := os.Stat(m.path); serr == nil {
		m.baseInfo = st
	}
	return nil
}

func (m *Map) applyHeaderLocked(h baseHeader) {
	m.nextMapUID = h.NextMapUID
	if m.nextMapUID == 0 {
		m.nextMapUID = 1
	}
	m.highestFileID = h.HighestFileID
	m.rebuildCount = h.RebuildCount
	m.createFileID = h.CreateFileID
	m.createTime = h.CreateTime
	m.indexID = h.IndexID
	m.lineage = h.Lineage
	m.foldedLineage = h.FoldedLineage
	m.foldedOffset = int64(h.FoldedOffset)
}

func (m *Map) headerLocked() baseHeader {
	return baseHeader{
		Version:       baseVersion2,
		RecordSize:    baseRecordLen,
		RecordCount:   uint32(m.st.count()),
		NextMapUID:    m.nextMapUID,
		HighestFileID: m.highestFileID,
		RebuildCount:  m.rebuildCount,
		CreateFileID:  m.createFileID,
		CreateTime:    m.createTime,
		FoldedOffset:  uint64(m.foldedOffset),
		FoldedLineage: m.foldedLineage,
		Lineage:       m.lineage,
		RecordsDigest: digestRecords(m.st.recs),
		IndexID:       m.indexID,
	}
}

// createFresh writes a brand-new empty base. Used on first open and as the
// fallback after a corrupt file is moved aside by the admin rebuild flow.
func (m *Map) createFresh() error {
	m.indexID = uint32(time.Now().Unix())
	m.st = store{}
	m.loaded = true
	m.nextMapUID = 1
	m.highestFileID = 0
	m.lineage = 0
	m.foldedLineage = 0
	m.foldedOffset = 0
	return m.flushLocked()
}

// baseTmpSeq gives concurrent base writers unique tmp names.
var baseTmpSeq atomic.Uint64

// writeBaseLocked rewrites the whole base atomically (.tmp + rename), so a
// reader either sees the previous file or the new one, never a half-written mix.
func (m *Map) writeBaseLocked() error {
	if m.format == FormatV1 {
		return m.writeBaseV1Locked()
	}
	// One name per writer. A shared "<path>.tmp" makes two writers race for the
	// same file: the winner's rename consumes it and the loser's rename fails
	// with ENOENT on a write that had nothing wrong with it (#1575).
	tmp := fmt.Sprintf("%s.tmp.%d.%d", m.path, os.Getpid(), baseTmpSeq.Add(1))
	buf := make([]byte, 0, baseHeaderLen+len(m.st.recs))
	buf = append(buf, encodeBaseHeader(m.headerLocked())...)
	buf = append(buf, m.st.recs...)
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("mdboxmap/write: tmp: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mdboxmap/write: rename: %w", err)
	}
	m.onDisk = FormatV2
	return nil
}

// applyLogTailLocked brings the handle to the end of the log belonging to the
// base it now holds. Every branch that takes a new base ends here: returning
// without it hands the caller a state one transaction stale.
func (m *Map) applyLogTailLocked() error {
	applied, err := m.replayFromPersistedLocked()
	if errors.Is(err, errLogIndexMismatch) {
		// Log left over from a previous map at this path — discard it.
		_ = os.Remove(m.logPath())
		m.logSize = 0
		return nil
	} else if err != nil {
		return err
	}
	m.logSize = applied
	return nil
}

// sameBaseLocked reports whether st is the file the records came from: identity
// first, since every rewrite renames, then size and mtime for a writer that
// updates in place. At coarse granularity a rewrite shares a tick with the read.
func (m *Map) sameBaseLocked(st os.FileInfo) bool {
	if m.baseInfo == nil || st == nil {
		return false
	}
	return os.SameFile(m.baseInfo, st) &&
		st.Size() == m.baseInfo.Size() &&
		st.ModTime().Equal(m.baseInfo.ModTime())
}

// peekHeaderLocked reads the 80-byte header without the record area. Deciding
// whether a rewritten base needs reading at all costs one short positional read.
func (m *Map) peekHeaderLocked() (baseHeader, error) {
	f, err := os.Open(m.path)
	if err != nil {
		return baseHeader{}, fmt.Errorf("mdboxmap/peek: open: %w", err)
	}
	defer f.Close()
	buf := make([]byte, baseHeaderLen)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return baseHeader{}, fmt.Errorf("mdboxmap/peek: read: %w", err)
	}
	return decodeBaseHeader(buf)
}

// adoptFoldLocked takes a rewritten base without reading it, only when it is this
// handle's state folded flat: same log, no further, records hashing to memory.
// The digest is what keeps that honest without a list of the paths that rewrite.
func (m *Map) adoptFoldLocked(h baseHeader, baseInfo os.FileInfo) bool {
	if h.FoldedLineage != m.logLineage || m.logSize < int64(h.FoldedOffset) {
		return false
	}
	if h.RecordsDigest != digestRecords(m.st.recs) {
		return false
	}
	m.applyHeaderLocked(h)
	m.logLineage = h.Lineage
	m.logSize = 0
	m.baseInfo = baseInfo
	return true
}

// findExtLocked resolves a map_uid to its index in the record area.
func (m *Map) findLocked(uid uint32) (int, bool) {
	if !m.loaded {
		return 0, false
	}
	return m.st.find(uid)
}

// withMapLock runs fn under the per-process Mutex and the cross-process map X
// lock. The HoldsResource shortcut keeps re-entrant calls from the same
// goroutine from deadlocking on the cross-process lock (POP3 QUIT pattern).
func (m *Map) withMapLock(fn func() error) error {
	// Writers queue here first, before anyone reaches the lock service. Left
	// unmeasured, this time belongs to no counter and the totals cannot
	// reconcile with the window they were taken over.
	blocked := time.Now()
	m.mu.Lock()
	metricMapWriteBlocked.Observe(time.Since(blocked).Seconds())
	defer m.mu.Unlock()
	if m.locker == nil {
		// No lock service: there is no cross-process hold to report, and
		// reporting one would put an operator's eye on a lock that does not
		// exist in their deployment.
		return fn()
	}
	key := locks.MdboxMapKey(m.username)
	if m.locker.HoldsResource(key) {
		// Already ours: no round trip, so nothing waited.
		return timed(metricMapLockHold, fn)
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "mdbox-map"), 35*time.Second)
	defer cancel()
	start := time.Now()
	lk, err := locks.Acquire(ctx, m.locker, key, m.owner, 30*time.Second)
	metricMapLockAcquire.Observe(time.Since(start).Seconds())
	if err != nil {
		return fmt.Errorf("mdboxmap/lock: %w", err)
	}
	defer func() { _ = m.locker.Unlock(ctx, lk.ID) }()
	return timed(metricMapLockHold, fn)
}

// timed runs fn and records how long it took. Separate from the waiting above
// because the two answer different questions: one is the lock service under
// load, the other is this process's own work.
func timed(h prometheus.Observer, fn func() error) error {
	start := time.Now()
	err := fn()
	h.Observe(time.Since(start).Seconds())
	return err
}

// invalidateLocked drops the freshness stamps where a change could not be
// persisted: memory is ahead of the file, and the fast path would keep it so.
func (m *Map) invalidateLocked() {
	m.baseInfo = nil
	m.logSize = -1
}

// flushLocked rewrites the base and drops the log; caller MUST hold m.mu. Base
// first, carrying the offset it folded, or a crash between them doubles deltas.
func (m *Map) flushLocked() error {
	start := time.Now()
	defer func() {
		metricMapFlush.Inc()
		metricMapFlushSeconds.Observe(time.Since(start).Seconds())
	}()
	seq, size, err := m.logIdentityLocked()
	if err != nil {
		return err
	}
	m.foldedLineage, m.foldedOffset = seq, size
	m.lineage++
	if err := m.writeBaseLocked(); err != nil {
		return err
	}
	if err := os.Remove(m.logPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mdboxmap/flush: remove log: %w", err)
	}
	m.logLineage = m.lineage
	m.logSize = 0
	if st, err := os.Stat(m.path); err == nil {
		m.baseInfo = st
	}
	return nil
}

// Compact folds the log into the base under the map lock. Folding only the folder
// indexes leaves every delivery here for the first open of every session.
func (m *Map) Compact() error {
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		return m.flushLocked()
	})
}

// JournalSizes reports the on-disk size of the base index and of the append
// log. A log that has been folded away does not exist and reports -1, which is
// the state a successful Compact leaves and is not the same as an empty log.
func (m *Map) JournalSizes() (int64, int64) {
	return statSize(m.path), statSize(m.logPath())
}

func statSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

// RebuildCount returns the persisted storage-wide-rebuild generation counter.
func (m *Map) RebuildCount() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebuildCount
}

// RebuildCountUnderCallersLock re-reads the map and answers, for a caller that
// already holds the cross-process map key: another pod's rebuild has to be
// visible, and re-taking the key here would rely on re-entrancy.
func (m *Map) RebuildCountUnderCallersLock() (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadLocked(); err != nil {
		return 0, err
	}
	return m.rebuildCount, nil
}

// CreateTime returns the persisted creation stamp of fileID, known only for the
// current append file. A file whose age cannot be proven is rotated by size
// only, never by age.
func (m *Map) CreateTime(fileID uint32) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fileID == 0 || fileID != m.createFileID {
		return 0, false
	}
	return int64(m.createTime), true
}

// RecordFileCreated persists fileID as the current append file with stamp ts,
// under the map lock, so the age check has a restart-safe anchor. A no-op when
// it already matches.
func (m *Map) RecordFileCreated(fileID uint32, ts int64) error {
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		if m.createFileID == fileID {
			return nil
		}
		m.createFileID = fileID
		m.createTime = uint64(ts)
		return m.flushLocked()
	})
}

// BumpRebuildCount increments and persists the storage-wide-rebuild generation
// counter under the cross-process map lock. Called once per successful rebuild.
func (m *Map) BumpRebuildCount() error {
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		m.rebuildCount++
		return m.flushLocked()
	})
}

// reloadLocked refreshes incrementally: the base only when changed, else the log
// tail. Caller holds m.mu; readers need no cross-process lock, a torn tail stops.
func (m *Map) reloadLocked() error {
	whole := time.Now()
	m.inReload = true
	defer func() {
		m.inReload = false
		metricMapReloadSeconds.Observe(time.Since(whole).Seconds())
	}()

	statStart := time.Now()
	baseStat, baseErr := os.Stat(m.path)
	var logSize int64
	if st, _ := os.Stat(m.logPath()); st != nil {
		logSize = st.Size()
	}
	m.observePart("stat", time.Since(statStart))

	sameBase := m.loaded && m.sameBaseLocked(baseStat)
	// A log shrinks only by being folded, so it signals change without a clock --
	// a rewrite inside one timestamp tick leaves the mtime where it was.
	logShrank := logSize < m.logSize

	// Fast path: nothing changed on disk.
	if sameBase && !logShrank && logSize == m.logSize {
		metricMapReload.WithLabelValues("fast").Inc()
		return nil
	}

	// The base file moved. Either it was rewritten by someone, or this is the
	// first load; the 80-byte header says which, and whether the records it now
	// holds are the ones already in memory.
	if !m.loaded || !sameBase || logShrank {
		if baseErr != nil {
			return fmt.Errorf("mdboxmap/reload: %w", baseErr)
		}
		if m.loaded && m.onDisk == FormatV2 && m.format == FormatV2 {
			h, herr := m.peekHeaderLocked()
			if herr != nil {
				return fmt.Errorf("mdboxmap/reload: %w", herr)
			}
			if m.adoptFoldLocked(h, baseStat) {
				metricMapReload.WithLabelValues("fold").Inc()
				// The writer that folded may already have appended to the log of
				// the lineage it started; leaving here carries that tail into
				// the next check, which is what purge runs before unlinking.
				return m.applyLogTailLocked()
			}
		}
		metricMapReload.WithLabelValues("reopen").Inc()
		if err := m.readBaseLocked(); err != nil {
			return fmt.Errorf("mdboxmap/reload: %w", err)
		}
		return m.applyLogTailLocked()
	}

	// Base unchanged, log grew: replay only the new tail.
	if logSize > m.logSize {
		metricMapReload.WithLabelValues("replay").Inc()
		applied, err := m.replayLogLocked(m.logSize)
		if err != nil && !errors.Is(err, errLogIndexMismatch) {
			return err
		}
		// replayLogLocked returns an offset no smaller than the one it was
		// given, but that is an invariant of another file, and a negative Add
		// panics.
		if delta := applied - m.logSize; delta > 0 {
			metricMapReplayBytes.Add(float64(delta))
		}
		m.logSize = applied
	}
	return nil
}

// HighestFileID returns the cached highest_file_id. No lock needed; the value is
// for diagnostics only. Trust the value returned by Append.Finish() for write
// decisions.
func (m *Map) HighestFileID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.highestFileID
}

// NextMapUID returns the next map_uid the index would assign on
// AppendBatch.Finish. Same caveat as HighestFileID: diagnostic only, the
// canonical value comes back from Finish.
func (m *Map) NextMapUID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextMapUID
}

// MessageCount returns the live record count (not the high-water map_uid).
// Exposed for tests and rebuild flows.
func (m *Map) MessageCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return 0
	}
	return m.st.count()
}

// looksForeignMapBase separates a legacy-named file of ours from theirs, which
// the name and a clean parse do not: ours has a "guid" extension and a 20-byte
// "map" header against their 8. Unparseable counts as theirs.
func looksForeignMapBase(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("mdboxmap/load: read %s: %w", path, err)
	}
	if bytes.HasPrefix(raw, []byte(baseMagic)) {
		return false, nil
	}
	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		return true, nil
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		return true, nil
	}
	if _, ok := dboxindex.Find(exts, extGUID); ok {
		return false, nil
	}
	if mapExt, ok := dboxindex.Find(exts, extMap); ok && len(mapExt.HeaderData) == mapHeaderSize {
		return false, nil
	}
	return true, nil
}
