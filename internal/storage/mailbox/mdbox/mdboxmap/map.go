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

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
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

	mu sync.Mutex
	f  *mailindex.File // in-memory mirror; nil after Close

	// nextMapUID is the next UID the mailindex record-stream would assign on
	// Append. Tracked explicitly so it can be published back to callers at
	// allocation time, not after the next Recreate.
	nextMapUID uint32

	// highestFileID is the latest value parsed from the "map" extension header;
	// updated on every AppendBatch that adds new m.<N> files.
	highestFileID uint32

	// rebuildCount is the storage-wide-rebuild generation counter parsed from the
	// "map" extension header. Bumped once per successful rebuild.
	rebuildCount uint32

	// createFileID / createTime record the id and unix-second creation stamp of
	// the current append file (the one Save writes into), persisted in the "map"
	// header for the mdbox_rotate_interval age check. Persisting the stamp keeps
	// it restart-safe without depending on an unreliable-over-NFS filesystem
	// btime. Only the current append file's stamp is tracked; an already-rotated
	// file is never appended to again, so its age no longer matters.
	createFileID uint32
	createTime   uint64

	// rotateSize is the per-m.<N> size cap the batch append path enforces. 0
	// means the package default (defaultRotateSize).
	rotateSize uint32

	// byMapUID indexes records by UID for O(1) Lookup. Rebuilt on every
	// load/flush.
	byMapUID map[uint32]int

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
	if err := m.loadOrInit(); err != nil {
		return nil, err
	}
	return m, nil
}

// Close releases per-handle state. Idempotent, no I/O.
func (m *Map) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.f = nil
	m.byMapUID = nil
	return nil
}

// loadOrInit reads the file from disk or, when it does not yet exist, creates a
// fresh map index with the canonical extensions.
//
// Migration: if the yarilo-native file is absent but a legacy file exists in the
// same directory, it is renamed into place atomically. From then on only the
// yarilo-native file is read or written.
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
	f, err := mailindex.Open(m.path)
	if err != nil {
		return fmt.Errorf("mdboxmap/load: open: %w", err)
	}
	m.f = f
	if st, serr := os.Stat(m.path); serr == nil {
		m.baseMod = st.ModTime()
	}
	applied, rerr := m.replayLogLocked(0)
	if errors.Is(rerr, errLogIndexMismatch) {
		_ = os.Remove(m.logPath())
		applied = 0
	} else if rerr != nil {
		return rerr
	}
	m.logSize = applied
	m.reindex()
	return nil
}

// createFresh writes a brand-new map.index with both extensions registered and
// zero records. Used on first OpenUser and as the fallback after a corrupt file
// is moved aside by the admin rebuild flow.
func (m *Map) createFresh() error {
	indexID := uint32(time.Now().Unix())
	f, err := mailindex.NewFile(indexID, defaultExtensions(0))
	if err != nil {
		return fmt.Errorf("mdboxmap/create: NewFile: %w", err)
	}
	f.Header.NextUID = 1
	m.f = f
	m.highestFileID = 0
	m.nextMapUID = 1
	m.byMapUID = map[uint32]int{}
	if err := m.flushLocked(); err != nil {
		return err
	}
	return nil
}

// reindex rebuilds the byMapUID lookup table and refreshes the cached header
// counters from m.f. Caller must hold m.mu.
// reindex rebuilds the UID index. Timed as its own part of a freshness check:
// a replay of a few kilobytes still walks every record afterwards, and that
// cost is invisible in the replay number.
func (m *Map) reindex() {
	start := time.Now()
	m.reindexLocked()
	m.observePart("reindex", time.Since(start))
}

func (m *Map) reindexLocked() {
	idx := make(map[uint32]int, len(m.f.Records))
	var maxUID, maxFileID uint32
	for i, rec := range m.f.Records {
		idx[rec.UID] = i
		if rec.UID > maxUID {
			maxUID = rec.UID
		}
		if data, ok := rec.Ext[extMap]; ok {
			if fid, _, _, err := decodeMapExt(data); err == nil && fid > maxFileID {
				maxFileID = fid
			}
		}
	}
	m.byMapUID = idx

	// Derive the allocation counters from the records too, not just the base
	// header: log-replayed appends advance them past what the base header (written
	// before those appends) recorded.
	next := m.f.Header.NextUID
	if maxUID+1 > next {
		next = maxUID + 1
	}
	if next == 0 {
		next = 1
	}
	m.nextMapUID = next

	hfid := uint32(0)
	if ext := findExt(m.f.Extensions, extMap); ext != nil {
		hfid, m.rebuildCount, m.createFileID, m.createTime = decodeMapHeader(ext.HdrData)
	}
	if maxFileID > hfid {
		hfid = maxFileID
	}
	m.highestFileID = hfid
}

// findExt returns a pointer to the named extension in the slice, or nil if not
// found. The pointer lets callers mutate HdrData in place; they must call
// flushLocked afterwards for the change to land on disk.
func findExt(exts []mailindex.Extension, name string) *mailindex.Extension {
	for i := range exts {
		if exts[i].Name == name {
			return &exts[i]
		}
	}
	return nil
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

// flushLocked rewrites the whole base index from m.f and drops the append log:
// the base now holds the full state, so the log resets to empty. This is the
// compaction point and the full-state persist for refcount/purge/file-id
// allocation. Caller MUST hold m.mu.
func (m *Map) flushLocked() error {
	start := time.Now()
	defer func() {
		metricMapFlush.Inc()
		metricMapFlushSeconds.Observe(time.Since(start).Seconds())
	}()
	if err := m.setMapHeaderLocked(); err != nil {
		return err
	}
	m.f.Header.MessagesCount = uint32(len(m.f.Records))
	m.f.Header.NextUID = m.nextMapUID
	if _, err := mailindex.Recreate(m.f.ToRecreateInput(m.path)); err != nil {
		return fmt.Errorf("mdboxmap/flush: %w", err)
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

// setMapHeaderLocked writes highest_file_id + rebuild_count into the "map"
// extension header and recomputes Header.HeaderSize/RecordSize so Recreate
// accepts the file. It also migrates a legacy 4-byte header to 8 bytes in place
// (growing HdrSize), mirroring File.AddHeaderExtension's recompute. Idempotent:
// once the header is 8 bytes, repeated calls re-encode the same size and do not
// migrate again. Caller MUST hold m.mu.
func (m *Map) setMapHeaderLocked() error {
	ext := findExt(m.f.Extensions, extMap)
	if ext == nil {
		return fmt.Errorf("mdboxmap/flush: missing %q extension", extMap)
	}
	ext.HdrData = encodeMapHeader(m.highestFileID, m.rebuildCount, m.createFileID, m.createTime)
	ext.HdrSize = uint32(len(ext.HdrData)) // 20 bytes; grows a legacy 4/8-byte header
	layout, err := mailindex.ComputeRecordLayout(m.f.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/flush: layout: %w", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/flush: encode ext headers: %w", err)
	}
	m.f.Extensions = layout.Extensions
	m.f.Layout = layout
	m.f.Header.RecordSize = layout.RecordSize
	m.f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
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

// reloadLocked refreshes m from disk incrementally. It re-opens the base only
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
	if m.f != nil && baseMod.Equal(m.baseMod) && logSize == m.logSize {
		metricMapReload.WithLabelValues("fast").Inc()
		return nil
	}

	// Base changed (or first load): re-open it, then replay the whole log.
	if m.f == nil || !baseMod.Equal(m.baseMod) {
		metricMapReload.WithLabelValues("reopen").Inc()
		if baseErr != nil {
			return fmt.Errorf("mdboxmap/reload: %w", baseErr)
		}
		f, err := mailindex.Open(m.path)
		if err != nil {
			return fmt.Errorf("mdboxmap/reload: %w", err)
		}
		m.f = f
		m.baseMod = baseMod
		applied, err := m.replayLogLocked(0)
		if errors.Is(err, errLogIndexMismatch) {
			// Log left over from a previous map at this path — discard it.
			_ = os.Remove(m.logPath())
			m.logSize = 0
			m.reindex()
			return nil
		} else if err != nil {
			return err
		}
		metricMapReplayBytes.Add(float64(applied))
		m.logSize = applied
		m.reindex()
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
		m.reindex()
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
	if m.f == nil {
		return 0
	}
	return len(m.f.Records)
}
