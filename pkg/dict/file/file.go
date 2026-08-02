// Package file is a JSON-backed local-filesystem dict driver.
//
// One Dict instance maps to one file path, passed in Config.Settings["path"];
// per-user templating (%u/%h/%n/%d/%i) is the caller's job via pkg/dict/varexpand
// before Open.
//
// An in-process sync.RWMutex guards reads and mutations. The driver is NOT safe
// across processes — two binaries opening the same path clobber each other — so
// it is for single-process runs (CLI, dev, tests, single-pod helm); use the
// redis or sql drivers for state shared across pods.
//
// Format: one JSON document with a fixed envelope and an array of rows; the
// "version" tag is reserved for future migrations. []byte values are
// base64-encoded by encoding/json, so binary values survive round-trip.
//
// Writes are atomic via temp-file + fsync + rename: the on-disk file is either
// the pre-write or post-write state, never partial.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
)

func init() {
	dict.Register("file", New)
}

// New constructs a file dict. Required setting: "path" (string). The file need
// not exist yet — the first commit creates it and any missing parent
// directories (0700).
func New(cfg dict.Config) (dict.Dict, error) {
	pathAny, ok := cfg.Settings["path"]
	if !ok {
		return nil, errors.New("missing required setting \"path\"")
	}
	path, ok := pathAny.(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("setting \"path\" must be a non-empty string, got %T", pathAny)
	}
	d := &Dict{path: path, rows: map[string]row{}}
	return d, nil
}

const formatVersion = 1

type Dict struct {
	path   string
	mu     sync.RWMutex
	rows   map[string]row
	loaded bool
	closed atomic.Bool
}

type row struct {
	Values  [][]byte `json:"v"`
	Expires int64    `json:"exp,omitempty"` // unix seconds; 0 = no TTL
}

type envelope struct {
	Version int         `json:"version"`
	Entries []wireEntry `json:"entries"`
}

type wireEntry struct {
	Key string `json:"k"`
	row
}

func (d *Dict) Name() string                 { return "file" }
func (d *Dict) Wait(_ context.Context) error { return nil }

func (d *Dict) Close() error {
	d.closed.Store(true)
	d.mu.Lock()
	d.rows = nil
	d.loaded = false
	d.mu.Unlock()
	return nil
}

// loadLocked reads the on-disk file into d.rows once; subsequent calls are a
// no-op. d.mu (write) must be held.
func (d *Dict) loadLocked() error {
	if d.loaded {
		return nil
	}
	data, err := os.ReadFile(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.loaded = true
			return nil
		}
		return fmt.Errorf("read: %w", err)
	}
	if len(data) == 0 {
		d.loaded = true
		return nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	for _, e := range env.Entries {
		d.rows[e.Key] = e.row
	}
	d.loaded = true
	return nil
}

// flushLocked serialises d.rows and writes it atomically via temp-file + fsync +
// rename. d.mu (write) must be held; d.rows must already be the desired
// post-write state.
func (d *Dict) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	entries := make([]wireEntry, 0, len(d.rows))
	for k, r := range d.rows {
		entries = append(entries, wireEntry{Key: k, row: r})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	data, err := json.MarshalIndent(envelope{Version: formatVersion, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(d.path), filepath.Base(d.path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck — best-effort cleanup if rename succeeds.

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, d.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func (d *Dict) Lookup(ctx context.Context, _ *dict.OpSettings, key string) ([][]byte, bool, error) {
	if err := d.guard(ctx); err != nil {
		return nil, false, err
	}
	d.mu.Lock() // write lock: loadLocked mutates state
	defer d.mu.Unlock()
	if err := d.loadLocked(); err != nil {
		return nil, false, err
	}
	r, ok := d.rows[key]
	if !ok {
		return nil, false, nil
	}
	if r.Expires > 0 && time.Now().Unix() > r.Expires {
		return nil, false, nil
	}
	out := make([][]byte, len(r.Values))
	for i, v := range r.Values {
		out[i] = append([]byte(nil), v...)
	}
	return out, true, nil
}

func (d *Dict) Iterate(ctx context.Context, _ *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	recurse := flags&dict.IterRecurse != 0
	exactKey := flags&dict.IterExactKey != 0
	noValue := flags&dict.IterNoValue != 0
	sortByKey := flags&dict.IterSortByKey != 0
	sortByValue := flags&dict.IterSortByValue != 0

	d.mu.Lock()
	if err := d.loadLocked(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	now := time.Now().Unix()
	var rows []iterRow
	for k, r := range d.rows {
		if r.Expires > 0 && now > r.Expires {
			continue
		}
		if !dict.PathMatches(path, k, recurse, exactKey) {
			continue
		}
		rr := iterRow{key: k}
		if !noValue {
			rr.values = make([][]byte, len(r.Values))
			for i, v := range r.Values {
				rr.values[i] = append([]byte(nil), v...)
			}
		}
		rows = append(rows, rr)
	}
	d.mu.Unlock()

	switch {
	case sortByKey:
		sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	case sortByValue:
		sort.Slice(rows, func(i, j int) bool {
			a, b := "", ""
			if len(rows[i].values) > 0 {
				a = string(rows[i].values[0])
			}
			if len(rows[j].values) > 0 {
				b = string(rows[j].values[0])
			}
			return a < b
		})
	}
	return &iterator{rows: rows, idx: -1}, nil
}

func (d *Dict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	expire := int64(0)
	if set != nil && set.ExpireSecs > 0 {
		expire = time.Now().Unix() + int64(set.ExpireSecs)
	}
	return &tx{d: d, expires: expire}, nil
}

func (d *Dict) ExpireScan(ctx context.Context) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.loadLocked(); err != nil {
		return err
	}
	now := time.Now().Unix()
	changed := false
	for k, r := range d.rows {
		if r.Expires > 0 && now > r.Expires {
			delete(d.rows, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return d.flushLocked()
}

func (d *Dict) guard(ctx context.Context) error {
	if d.closed.Load() {
		return dict.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// ----- iterator -----

type iterRow struct {
	key    string
	values [][]byte
}

type iterator struct {
	rows []iterRow
	idx  int
	err  error
}

func (it *iterator) Next() bool {
	it.idx++
	return it.idx < len(it.rows)
}

func (it *iterator) Key() string { return it.rows[it.idx].key }

func (it *iterator) Values() [][]byte {
	if it.idx < 0 || it.idx >= len(it.rows) {
		return nil
	}
	return it.rows[it.idx].values
}

func (it *iterator) Err() error   { return it.err }
func (it *iterator) Close() error { return nil }

// ----- tx -----

type tx struct {
	d       *Dict
	buf     dict.MemoryTx
	expires int64
	done    bool
}

func (t *tx) Set(key string, value []byte) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.Set(key, value)
	return nil
}

func (t *tx) Unset(key string) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.Unset(key)
	return nil
}

func (t *tx) AtomicInc(key string, delta int64) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.AtomicInc(key, delta)
	return nil
}

func (t *tx) Rollback() error {
	t.done = true
	t.buf.Reset()
	return nil
}

func (t *tx) Commit() (dict.CommitResult, error) {
	if t.done {
		return dict.CommitFailed, errors.New("file: tx already finalised")
	}
	t.done = true
	if t.d.closed.Load() {
		return dict.CommitFailed, dict.ErrClosed
	}

	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	if err := t.d.loadLocked(); err != nil {
		return dict.CommitFailed, err
	}

	// Apply ops to a snapshot — only commit to t.d.rows after every op
	// succeeds, so a failing atomic-inc does not leave a half-applied
	// transaction in memory.
	snap := make(map[string]row, len(t.d.rows))
	for k, v := range t.d.rows {
		snap[k] = v
	}

	for _, op := range t.buf.Ops {
		switch op.Kind {
		case dict.OpSet:
			r := row{Values: [][]byte{append([]byte(nil), op.Value...)}, Expires: t.expires}
			snap[op.Key] = r
		case dict.OpUnset:
			delete(snap, op.Key)
		case dict.OpAtomicInc:
			r, ok := snap[op.Key]
			if !ok || len(r.Values) == 0 {
				return dict.CommitNotFound, nil
			}
			n, err := strconv.ParseInt(string(r.Values[0]), 10, 64)
			if err != nil {
				return dict.CommitFailed, fmt.Errorf("file: atomic-inc on non-integer value at %q", op.Key)
			}
			r.Values = [][]byte{[]byte(strconv.FormatInt(n+op.Delta, 10))}
			if t.expires > 0 {
				r.Expires = t.expires
			}
			snap[op.Key] = r
		}
	}

	// Promote snapshot then flush. On flush failure, the in-memory map
	// is rewound so subsequent reads still see the pre-commit state.
	prev := t.d.rows
	t.d.rows = snap
	if err := t.d.flushLocked(); err != nil {
		t.d.rows = prev
		return dict.CommitFailed, err
	}
	return dict.CommitOK, nil
}
