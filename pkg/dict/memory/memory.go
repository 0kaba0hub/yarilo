// Package memory is an in-process dict driver backed by a sync.Map.
//
// Intended use: unit tests and short-lived dev runs of the standalone
// binary where persistence is not required. State is lost on process
// exit. NOT suitable for production: gives no durability guarantees,
// no cross-process visibility (use file/redis/sql for that).
//
// Configuration: no settings — instantiating with Driver:"memory" is
// enough. TTL is honoured at lookup/iterate time (lazy expiration);
// ExpireScan walks the map and purges anything past its deadline.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
)

func init() {
	dict.Register("memory", New)
}

// New constructs a memory dict. The Config is ignored — there is
// nothing to configure for an in-process map.
func New(_ dict.Config) (dict.Dict, error) {
	return &Dict{}, nil
}

// Dict is the memory driver. All methods are goroutine-safe.
type Dict struct {
	mu     sync.RWMutex
	rows   map[string]row
	closed atomic.Bool
}

type row struct {
	values  [][]byte
	expires time.Time // zero = no TTL
}

func (d *Dict) Name() string { return "memory" }

func (d *Dict) Close() error {
	d.closed.Store(true)
	d.mu.Lock()
	d.rows = nil
	d.mu.Unlock()
	return nil
}

func (d *Dict) Wait(_ context.Context) error { return nil }

func (d *Dict) Lookup(ctx context.Context, _ *dict.OpSettings, key string) ([][]byte, bool, error) {
	if err := d.guard(ctx); err != nil {
		return nil, false, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	r, ok := d.rows[key]
	if !ok {
		return nil, false, nil
	}
	if !r.expires.IsZero() && time.Now().After(r.expires) {
		return nil, false, nil
	}
	// Defensive copy so caller mutations cannot bleed into our map.
	out := make([][]byte, len(r.values))
	for i, v := range r.values {
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

	now := time.Now()
	d.mu.RLock()
	var keys []string
	for k, r := range d.rows {
		if !r.expires.IsZero() && now.After(r.expires) {
			continue
		}
		if dict.PathMatches(path, k, recurse, exactKey) {
			keys = append(keys, k)
		}
	}
	rows := make([]iterRow, 0, len(keys))
	for _, k := range keys {
		r := d.rows[k]
		rr := iterRow{key: k}
		if !noValue {
			rr.values = make([][]byte, len(r.values))
			for i, v := range r.values {
				rr.values[i] = append([]byte(nil), v...)
			}
		}
		rows = append(rows, rr)
	}
	d.mu.RUnlock()

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
	expire := time.Duration(0)
	if set != nil && set.ExpireSecs > 0 {
		expire = time.Duration(set.ExpireSecs) * time.Second
	}
	return &tx{d: d, expire: expire}, nil
}

func (d *Dict) ExpireScan(ctx context.Context) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, r := range d.rows {
		if !r.expires.IsZero() && now.After(r.expires) {
			delete(d.rows, k)
		}
	}
	return nil
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

func (it *iterator) Err() error { return it.err }

func (it *iterator) Close() error { return nil }

// ----- tx -----

type tx struct {
	d      *Dict
	buf    dict.MemoryTx
	expire time.Duration
	done   bool
}

func (t *tx) Set(key string, value []byte) error {
	if t.done {
		return errors.New("memory: tx already finalised")
	}
	t.buf.Set(key, value)
	return nil
}

func (t *tx) Unset(key string) error {
	if t.done {
		return errors.New("memory: tx already finalised")
	}
	t.buf.Unset(key)
	return nil
}

func (t *tx) AtomicInc(key string, delta int64) error {
	if t.done {
		return errors.New("memory: tx already finalised")
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
		return dict.CommitFailed, errors.New("memory: tx already finalised")
	}
	t.done = true
	if t.d.closed.Load() {
		return dict.CommitFailed, dict.ErrClosed
	}

	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	if t.d.rows == nil {
		t.d.rows = make(map[string]row)
	}
	var expires time.Time
	if t.expire > 0 {
		expires = time.Now().Add(t.expire)
	}
	for _, op := range t.buf.Ops {
		switch op.Kind {
		case dict.OpSet:
			t.d.rows[op.Key] = row{
				values:  [][]byte{append([]byte(nil), op.Value...)},
				expires: expires,
			}
		case dict.OpUnset:
			delete(t.d.rows, op.Key)
		case dict.OpAtomicInc:
			r, ok := t.d.rows[op.Key]
			if !ok || len(r.values) == 0 {
				return dict.CommitNotFound, nil
			}
			n, err := strconv.ParseInt(string(r.values[0]), 10, 64)
			if err != nil {
				return dict.CommitFailed, fmt.Errorf("memory: atomic-inc on non-integer value at %q", op.Key)
			}
			r.values = [][]byte{[]byte(strconv.FormatInt(n+op.Delta, 10))}
			if t.expire > 0 {
				r.expires = expires
			}
			t.d.rows[op.Key] = r
		}
	}
	return dict.CommitOK, nil
}
