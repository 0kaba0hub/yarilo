package mdboxmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// Map is the in-memory + on-disk handle for one user's map index. All mutations
// route through Map so the in-process Mutex and cross-process X lock stay
// coherent.
//
// Map is not goroutine-safe outside its own methods; share a single Map per
// user-session, do not pass it to a worker pool without external
// synchronisation.
type Map struct {
	path     string
	username string
	owner    string
	locker   locks.Locker

	mu     sync.Mutex
	st     store
	loaded bool

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

	// createFileID / createTime record the id and unix-second creation stamp of
	// the current append file (the one Save writes into) for the
	// mdbox_rotate_interval age check. Persisting the stamp keeps it
	// restart-safe without depending on an unreliable-over-NFS filesystem btime.
	// Only the current append file's stamp is tracked; an already-rotated file is
	// never appended to again, so its age no longer matters.
	createFileID uint32
	createTime   uint64

	// indexID pairs the base with its append log; a log carrying a different id
	// belongs to an earlier map at this path and is discarded, never applied.
	indexID uint32

	// logSeq / logReplayOffset are the persisted "how far into the log does the
	// base already reach" pair. The offset is honoured only when the log on disk
	// carries logSeq: a log from an earlier base is replayed whole instead.
	logSeq          uint32
	logReplayOffset int64

	// rotateSize is the per-m.<N> size cap the batch append path enforces. 0
	// means the package default (defaultRotateSize).
	rotateSize uint32

	// baseMod / logSize track what this handle has applied from disk so
	// reloadLocked can fast-path when nothing changed and replay only the log
	// tail a sibling process appended since. baseMod is the base index file's
	// mtime; logSize is the replayed byte offset of the append log.
	baseMod time.Time
	logSize int64
	// inReload is true while a freshness check is running, so its parts are
	// timed against a whole that exists. Guarded by m.mu like the rest.
	inReload bool
}

// Option configures Map construction.
type Option func(*Map)

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

// rotateSizeOrDefault returns the configured rotate size, or the package default.
func (m *Map) rotateSizeOrDefault() uint32 {
	if m.rotateSize == 0 {
		return defaultRotateSize
	}
	return m.rotateSize
}

// Open opens (or creates) the per-user mdbox map at dir. The canonical filename
// is MapIndexFileName ("yarilo.map.index"). On first open it also probes for
// LegacyMapIndexFileName and migrates it in place (see loadOrInit). username is
// the cross-process map-lock key (see locks.MdboxMapKey).
func Open(dir, username string, opts ...Option) (*Map, error) {
	m := &Map{
		path:     filepath.Join(dir, MapIndexFileName),
		username: username,
	}
	for _, opt := range opts {
		opt(m)
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

// loadOrInit reads the base from disk or, when it does not yet exist, creates a
// fresh one.
//
// Two file-level transitions happen here, both once per user. A legacy-named
// file is renamed into place; a v1-format base is converted to v2 (see
// convert.go). Anything else the version byte does not name is refused: this
// index decides which physical bytes belong to which message and which file a
// purge may unlink, so a misparse is mail loss.
func (m *Map) loadOrInit() error {
	if _, err := os.Stat(m.path); errors.Is(err, os.ErrNotExist) {
		legacy := filepath.Join(filepath.Dir(m.path), LegacyMapIndexFileName)
		if _, lerr := os.Stat(legacy); lerr == nil {
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
		if cerr := m.convertV1Locked(); cerr != nil {
			return cerr
		}
		err = m.readBaseLocked()
	}
	metricMapOpenPart.WithLabelValues("base").Observe(time.Since(tBase).Seconds())
	if err != nil {
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
	m.applyHeaderLocked(h)
	if st, serr := os.Stat(m.path); serr == nil {
		m.baseMod = st.ModTime()
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
	m.logSeq = h.LogSeq
	m.logReplayOffset = int64(h.LogReplayOffset)
}

func (m *Map) headerLocked() baseHeader {
	return baseHeader{
		Version:         baseVersion2,
		RecordSize:      baseRecordLen,
		RecordCount:     uint32(m.st.count()),
		NextMapUID:      m.nextMapUID,
		HighestFileID:   m.highestFileID,
		RebuildCount:    m.rebuildCount,
		CreateFileID:    m.createFileID,
		CreateTime:      m.createTime,
		LogReplayOffset: uint64(m.logReplayOffset),
		IndexID:         m.indexID,
		LogSeq:          m.logSeq,
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
	m.logSeq = 0
	m.logReplayOffset = 0
	return m.flushLocked()
}

// writeBaseLocked rewrites the whole base atomically (.tmp + rename), so a
// reader either sees the previous file or the new one, never a half-written mix.
func (m *Map) writeBaseLocked() error {
	tmp := m.path + ".tmp"
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
	return nil
}

// writeLogPairingLocked records which log, and how much of it, the base on disk
// already contains. Only those two fields are touched: everything else in the
// header describes the records in the file, and memory is routinely ahead of
// them — an append lives in the log long before the base is rewritten, so
// publishing the in-memory counts here would claim records the file does not
// hold.
func (m *Map) writeLogPairingLocked() error {
	f, err := os.OpenFile(m.path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("mdboxmap/header: open: %w", err)
	}
	defer f.Close()
	buf := make([]byte, baseHeaderLen)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("mdboxmap/header: read: %w", err)
	}
	h, err := decodeBaseHeader(buf)
	if err != nil {
		return err
	}
	h.LogSeq = m.logSeq
	h.LogReplayOffset = uint64(m.logReplayOffset)
	if _, err := f.WriteAt(encodeBaseHeader(h), 0); err != nil {
		return fmt.Errorf("mdboxmap/header: write: %w", err)
	}
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := time.Now()
	lk, err := locks.Acquire(ctx, m.locker, key, m.owner, 30*time.Second)
	metricMapLockWait.Observe(time.Since(start).Seconds())
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

// invalidateLocked drops the freshness stamps so the next read reloads from
// disk instead of trusting memory. Used where an in-memory change could not be
// persisted: memory is then ahead of the file, and the fast path would keep it
// that way.
func (m *Map) invalidateLocked() {
	m.baseMod = time.Time{}
	m.logSize = -1
}

// flushLocked rewrites the whole base from memory and drops the append log: the
// base now holds the full state. This is the compaction point and the
// full-state persist for refcount/purge/file-id allocation. Caller MUST hold
// m.mu.
//
// The order matters. The base is written first, carrying the log offset it
// already incorporates, and only then is the log removed: a crash in between
// leaves a base that knows the log is folded in, so the next open replays
// nothing from it. Replaying it would double-apply every refcount delta it
// carries.
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
	if seq != 0 {
		m.logSeq = seq
	}
	m.logReplayOffset = size
	if err := m.writeBaseLocked(); err != nil {
		return err
	}
	if err := os.Remove(m.logPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mdboxmap/flush: remove log: %w", err)
	}
	m.logSize = 0
	if st, err := os.Stat(m.path); err == nil {
		m.baseMod = st.ModTime()
	}
	return nil
}

// RebuildCount returns the persisted storage-wide-rebuild generation counter.
func (m *Map) RebuildCount() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebuildCount
}

// CreateTime returns the persisted unix-second creation stamp of file fileID and
// whether it is known. Only the current append file's stamp is tracked, so it
// returns ok=false for any other (already-rotated or legacy) file; the caller
// then skips the age-based rotation check (a file whose age cannot be proven is
// rotated by size only, never by age).
func (m *Map) CreateTime(fileID uint32) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fileID == 0 || fileID != m.createFileID {
		return 0, false
	}
	return int64(m.createTime), true
}

// RecordFileCreated persists fileID as the current append file with creation
// stamp ts (unix seconds), under the cross-process map lock. Called once when a
// new physical m.<N> file is first written (Save's first record, or a compaction
// destination) so the mdbox_rotate_interval age check has a restart-safe anchor.
// A no-op when fileID already matches the recorded current file.
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

// reloadLocked refreshes m from disk incrementally. It re-reads the base only
// when it changed (compaction / full-state rewrite) and otherwise replays just
// the append-log tail a sibling process wrote since our last apply, so a peer's
// deliveries become visible without re-reading the whole map. Caller MUST hold
// m.mu. Write callers additionally hold the cross-process lock; readers may call
// it lock-free (a torn log tail is stopped cleanly by replayLogLocked).
func (m *Map) reloadLocked() error {
	whole := time.Now()
	m.inReload = true
	defer func() {
		m.inReload = false
		metricMapReloadSeconds.Observe(time.Since(whole).Seconds())
	}()

	statStart := time.Now()
	var baseMod time.Time
	baseStat, baseErr := os.Stat(m.path)
	if baseStat != nil {
		baseMod = baseStat.ModTime()
	}
	var logSize int64
	if st, _ := os.Stat(m.logPath()); st != nil {
		logSize = st.Size()
	}
	m.observePart("stat", time.Since(statStart))

	// Fast path: nothing changed on disk.
	if m.loaded && baseMod.Equal(m.baseMod) && logSize == m.logSize {
		metricMapReload.WithLabelValues("fast").Inc()
		return nil
	}

	// Base changed (or first load): re-read it, then replay from the offset it
	// records as already folded in.
	if !m.loaded || !baseMod.Equal(m.baseMod) {
		metricMapReload.WithLabelValues("reopen").Inc()
		if baseErr != nil {
			return fmt.Errorf("mdboxmap/reload: %w", baseErr)
		}
		if err := m.readBaseLocked(); err != nil {
			return fmt.Errorf("mdboxmap/reload: %w", err)
		}
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
