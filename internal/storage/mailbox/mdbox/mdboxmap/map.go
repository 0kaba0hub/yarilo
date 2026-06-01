package mdboxmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// Map is the in-memory + on-disk handle for one user's
// dovecot.map.index. All mutations route through Map so the
// in-process Mutex + cross-process X lock stay coherent.
//
// Map is NOT goroutine-safe outside its own methods; share a
// single Map per user-session, do not pass to a worker pool
// without external synchronisation.
type Map struct {
	path     string
	username string
	owner    string
	locker   locks.Locker

	mu sync.Mutex
	f  *mailindex.File // in-memory mirror; nil after Close

	// nextMapUID is the next UID the mailindex's record-stream
	// would assign on Append. We track it explicitly because we
	// need to publish it back to callers AT allocation time, not
	// after the next Recreate.
	nextMapUID uint32

	// highestFileID is the latest value parsed from the "map"
	// extension header; updated on every AppendBatch that adds
	// new m.<N> files.
	highestFileID uint32

	// byMapUID indexes records by UID for O(1) Lookup.
	// Rebuilt on every load/flush.
	byMapUID map[uint32]int
}

// Option configures Map construction.
type Option func(*Map)

// WithLocker wires a yarilo-locks client. A nil Locker leaves
// only the in-process Mutex as the barrier — never safe in k8s
// production, only for unit tests.
func WithLocker(l locks.Locker) Option {
	return func(m *Map) { m.locker = l }
}

// WithOwner sets the lock-owner string surfaced to yarilo-locks.
// Defaults to "<process>/<pid>/<user>" when unset.
func WithOwner(s string) Option {
	return func(m *Map) { m.owner = s }
}

// Open opens (or creates) the per-user mdbox map at dir. The
// canonical filename is MapIndexFileName ("yarilo.map.index").
// On first open we also probe for LegacyMapIndexFileName
// ("dovecot.map.index") and migrate it in place — see
// loadOrInit. username is the cross-process map-lock key (see
// locks.MdboxMapKey).
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

// Close releases per-handle state. Idempotent. No I/O.
func (m *Map) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.f = nil
	m.byMapUID = nil
	return nil
}

// loadOrInit reads the file from disk or, when it does not yet
// exist, creates a fresh map index with the canonical extensions.
//
// Migration path: if the yarilo-native file is absent but a
// legacy file is found in the same directory, we rename it into
// place atomically. From that point on, only the yarilo-native
// file is read or written.
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
	m.reindex()
	return nil
}

// createFresh writes a brand-new map.index with both extensions
// registered and zero records. Used both on first OpenUser and
// as the fallback after a corrupt file is moved aside by the
// admin rebuild flow.
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

// reindex rebuilds the byMapUID lookup table and refreshes the
// cached header counters from m.f. Caller must hold m.mu.
func (m *Map) reindex() {
	idx := make(map[uint32]int, len(m.f.Records))
	for i, rec := range m.f.Records {
		idx[rec.UID] = i
	}
	m.byMapUID = idx
	m.nextMapUID = m.f.Header.NextUID
	if m.nextMapUID == 0 {
		m.nextMapUID = 1
	}
	if ext := findExt(m.f.Extensions, extMap); ext != nil {
		m.highestFileID = decodeMapHeader(ext.HdrData)
	}
}

// findExt returns a pointer to the named extension in the slice,
// or nil if not found. Returns a pointer so callers can mutate
// HdrData in place; the caller must call flushLocked afterwards
// for the change to land on disk.
func findExt(exts []mailindex.Extension, name string) *mailindex.Extension {
	for i := range exts {
		if exts[i].Name == name {
			return &exts[i]
		}
	}
	return nil
}

// withMapLock runs fn under the per-process Mutex + the
// cross-process map X lock. The HoldsResource shortcut keeps
// re-entrant calls from the same goroutine from deadlocking on
// the cross-process lock (POP3 QUIT pattern).
func (m *Map) withMapLock(fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locker == nil {
		return fn()
	}
	key := locks.MdboxMapKey(m.username)
	if m.locker.HoldsResource(key) {
		return fn()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, m.locker, key, m.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("mdboxmap/lock: %w", err)
	}
	defer func() { _ = m.locker.Unlock(ctx, lk.ID) }()
	return fn()
}

// flushLocked rewrites the on-disk map.index file from m.f.
// Caller MUST hold m.mu. Used by every mutation path.
func (m *Map) flushLocked() error {
	if ext := findExt(m.f.Extensions, extMap); ext != nil {
		ext.HdrData = encodeMapHeader(m.highestFileID)
	}
	m.f.Header.MessagesCount = uint32(len(m.f.Records))
	m.f.Header.NextUID = m.nextMapUID
	if _, err := mailindex.Recreate(m.f.ToRecreateInput(m.path)); err != nil {
		return fmt.Errorf("mdboxmap/flush: %w", err)
	}
	return nil
}

// reloadLocked re-reads the on-disk file into m. Used after
// taking the cross-process lock when a peer may have modified
// the map; without this, two processes hand out colliding
// map_uids. Caller MUST hold m.mu and the cross-process lock.
func (m *Map) reloadLocked() error {
	f, err := mailindex.Open(m.path)
	if err != nil {
		return fmt.Errorf("mdboxmap/reload: %w", err)
	}
	m.f = f
	m.reindex()
	return nil
}

// HighestFileID returns the cached highest_file_id. Caller does
// not need to hold any lock — value is exposed for diagnostics
// only; trust the value returned by Append.Finish() for write
// decisions.
func (m *Map) HighestFileID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.highestFileID
}

// NextMapUID returns the next map_uid the index would assign on
// AppendBatch.Finish. Same caveat as HighestFileID — diagnostic
// only; the canonical value comes back from Finish.
func (m *Map) NextMapUID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextMapUID
}

// MessageCount returns the live record count (not the high-water
// map_uid). Exposed for tests and rebuild flows.
func (m *Map) MessageCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.f == nil {
		return 0
	}
	return len(m.f.Records)
}
