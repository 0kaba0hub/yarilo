// Package redis is a Redis-backed dict driver for state shared across pods.
//
// Storage model: every dict key becomes one Redis string key (Set/Get/Del).
// Multi-value falls back to a single value per key; the SQL driver carries
// true multi-value when needed.
//
// Iteration: SCAN with a MATCH pattern derived from the iterate path. Returned
// keys are then filtered locally, since SCAN cannot stop at the first "/".
//
// Transactions: MULTI/EXEC. AtomicInc uses INCRBY; CommitNotFound is emulated
// by an EXISTS check before the MULTI (the race window is acceptable —
// concurrent unset on a counter is not a supported pattern).
//
// TTL: OpSettings.ExpireSecs becomes an EXPIRE in the same MULTI as the SET.
// ExpireScan is a no-op — Redis expires keys server-side.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

func init() {
	dict.Register("redis", New)
}

// New constructs a redis dict. Recognised settings:
//
//	"addr"     string         — "host:port" of the Redis server (required)
//	"password" string         — AUTH password; empty for no auth
//	"db"       int            — logical database number; default 0
//	"prefix"   string         — prepended to every dict key on the wire
//	"dial_timeout" string     — Go duration; default 5s
//
// "prefix" lets multiple dict configurations share one Redis instance without
// colliding (e.g. "yarilo:metadata:%u:", "yarilo:quota:"). The caller must have
// varexpand'd %u/%h/%n before Open.
func New(cfg dict.Config) (dict.Dict, error) {
	addr, _ := cfg.Settings["addr"].(string)
	if addr == "" {
		return nil, errors.New("missing required setting \"addr\"")
	}
	opts := &redis.Options{Addr: addr}
	if pw, ok := cfg.Settings["password"].(string); ok {
		opts.Password = pw
	}
	if db, ok := cfg.Settings["db"]; ok {
		switch v := db.(type) {
		case int:
			opts.DB = v
		case int64:
			opts.DB = int(v)
		case float64:
			opts.DB = int(v)
		}
	}
	if tStr, ok := cfg.Settings["dial_timeout"].(string); ok && tStr != "" {
		d, err := time.ParseDuration(tStr)
		if err != nil {
			return nil, fmt.Errorf("dial_timeout: %w", err)
		}
		opts.DialTimeout = d
	}
	prefix, _ := cfg.Settings["prefix"].(string)
	return &Dict{client: redis.NewClient(opts), prefix: prefix}, nil
}

type Dict struct {
	client *redis.Client
	prefix string
	closed atomic.Bool
}

func (d *Dict) Name() string                       { return "redis" }
func (d *Dict) ExpireScan(_ context.Context) error { return nil }
func (d *Dict) Wait(_ context.Context) error       { return nil }

func (d *Dict) Close() error {
	d.closed.Store(true)
	return d.client.Close()
}

func (d *Dict) key(set *dict.OpSettings, k string) string {
	return d.prefix + dict.ScopedKey(set, k)
}
func (d *Dict) trim(set *dict.OpSettings, k string) string {
	// strip the Redis prefix, then the per-user scope prefix if any
	k = strings.TrimPrefix(k, d.prefix)
	if set != nil && set.Username != "" {
		k = strings.TrimPrefix(k, set.Username+"/")
	}
	return k
}

func (d *Dict) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, dict.ErrClosed
	}
	v, err := d.client.Get(ctx, d.key(set, key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get: %w", err)
	}
	return [][]byte{v}, true, nil
}

func (d *Dict) Iterate(ctx context.Context, set *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	recurse := flags&dict.IterRecurse != 0
	exactKey := flags&dict.IterExactKey != 0
	noValue := flags&dict.IterNoValue != 0

	if exactKey {
		vals, found, err := d.Lookup(ctx, set, path)
		if err != nil {
			return nil, err
		}
		if !found {
			return &iterator{}, nil
		}
		var rows []iterRow
		if noValue {
			rows = []iterRow{{key: path}}
		} else {
			rows = []iterRow{{key: path, values: vals}}
		}
		return &iterator{rows: rows, idx: -1}, nil
	}

	pattern := d.key(set, path) + "*"
	var (
		cursor uint64
		rows   []iterRow
	)
	for {
		batch, next, err := d.client.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		for _, full := range batch {
			k := d.trim(set, full)
			if !dict.PathMatches(path, k, recurse, false) {
				continue
			}
			row := iterRow{key: k}
			if !noValue {
				v, err := d.client.Get(ctx, full).Bytes()
				if err == redis.Nil {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("get during scan: %w", err)
				}
				row.values = [][]byte{v}
			}
			rows = append(rows, row)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	applySortFlags(rows, flags)
	return &iterator{rows: rows, idx: -1}, nil
}

func (d *Dict) Begin(_ context.Context, set *dict.OpSettings) (dict.Tx, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	ttl := time.Duration(0)
	var username string
	if set != nil {
		if set.ExpireSecs > 0 {
			ttl = time.Duration(set.ExpireSecs) * time.Second
		}
		username = set.Username
	}
	return &tx{d: d, ttl: ttl, username: username}, nil
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

func applySortFlags(rows []iterRow, flags dict.IterFlag) {
	if flags&dict.IterSortByKey != 0 {
		sortByKey(rows)
	} else if flags&dict.IterSortByValue != 0 {
		sortByValue(rows)
	}
}

func sortByKey(rows []iterRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].key > rows[j].key; j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

func sortByValue(rows []iterRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := "", ""
			if len(rows[j-1].values) > 0 {
				a = string(rows[j-1].values[0])
			}
			if len(rows[j].values) > 0 {
				b = string(rows[j].values[0])
			}
			if a <= b {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// ----- tx -----

type tx struct {
	d        *Dict
	buf      dict.MemoryTx
	ttl      time.Duration
	username string
	done     bool
}

func (t *tx) Set(key string, value []byte) error {
	if t.done {
		return errors.New("redis: tx already finalised")
	}
	t.buf.Set(key, value)
	return nil
}

func (t *tx) Unset(key string) error {
	if t.done {
		return errors.New("redis: tx already finalised")
	}
	t.buf.Unset(key)
	return nil
}

func (t *tx) AtomicInc(key string, delta int64) error {
	if t.done {
		return errors.New("redis: tx already finalised")
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
		return dict.CommitFailed, errors.New("redis: tx already finalised")
	}
	t.done = true
	if t.d.closed.Load() {
		return dict.CommitFailed, dict.ErrClosed
	}

	ctx := context.Background()

	// Pre-check that every atomic-inc target exists: a missing key →
	// CommitNotFound, no ops applied. The EXISTS/MULTI race window is
	// acceptable — concurrent unset on a counter is not a supported pattern.
	set := &dict.OpSettings{Username: t.username}
	for _, op := range t.buf.Ops {
		if op.Kind != dict.OpAtomicInc {
			continue
		}
		n, err := t.d.client.Exists(ctx, t.d.key(set, op.Key)).Result()
		if err != nil {
			return dict.CommitFailed, fmt.Errorf("redis exists pre-check: %w", err)
		}
		if n == 0 {
			return dict.CommitNotFound, nil
		}
	}

	_, err := t.d.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, op := range t.buf.Ops {
			full := t.d.key(set, op.Key)
			switch op.Kind {
			case dict.OpSet:
				if t.ttl > 0 {
					p.Set(ctx, full, op.Value, t.ttl)
				} else {
					p.Set(ctx, full, op.Value, 0)
				}
			case dict.OpUnset:
				p.Del(ctx, full)
			case dict.OpAtomicInc:
				p.IncrBy(ctx, full, op.Delta)
				if t.ttl > 0 {
					p.Expire(ctx, full, t.ttl)
				}
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return dict.CommitNotFound, nil
		}
		// Network error mid-EXEC → write-uncertain: the server may have
		// applied the EXEC before the connection broke.
		if isNetworkErr(err) {
			return dict.CommitWriteUncertain, err
		}
		return dict.CommitFailed, err
	}
	return dict.CommitOK, nil
}

func isNetworkErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe")
}

// Atomic-inc against a non-integer value surfaces as a regular Commit error
// (INCRBY returns "value is not an integer"); callers do not distinguish it
// from any other commit failure.
var _ = strconv.ParseInt
