// Package redis is a Redis-backed dict driver.
//
// Designed for the yarilo backend k8s deployment where state must be
// shared across pods (a redis Service sits in the same namespace and
// every yarilo session pod talks to it). Standalone deployments can
// also use this driver — it scales from one pod to many without code
// change.
//
// Storage model: every dict key becomes a single Redis string key
// (Set/Get/Del). Multi-value semantics fall back to a single value
// per key — multi-value is rarely needed for yarilo's use cases
// (METADATA, quota counters) and a Redis LIST per key would complicate
// SCAN and EXPIRE without earning much. SQL driver carries true
// multi-value when needed.
//
// Iteration: SCAN with a MATCH pattern derived from the iterate path.
// We append "*" or "?*" depending on recursion flag. Returned keys are
// then locally filtered against the path-matching rule so that
// shallow iteration drops keys with extra "/" segments — Redis SCAN
// has no native "stop at first slash" filter.
//
// Transactions: Redis MULTI/EXEC provides atomic execution of
// queued commands. AtomicInc uses INCRBY on integer values; the
// CommitNotFound semantics are emulated by checking EXISTS before
// the MULTI (race-prone but matches Dovecot's contract — Dovecot's
// own dict-redis driver has the same WATCH/MULTI window).
//
// TTL: OpSettings.ExpireSecs translates to EXPIRE issued in the same
// MULTI block as the SET. ExpireScan is a no-op — Redis handles
// expiry server-side; the call returns immediately.
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
// "prefix" lets multiple dict configurations co-exist on the same
// Redis instance without colliding (e.g. "yarilo:metadata:%u:" for
// per-user metadata, "yarilo:quota:" for global counters). The caller
// is expected to have varexpand'd %u/%h/%n before Open.
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

func (d *Dict) key(k string) string { return d.prefix + k }
func (d *Dict) trim(k string) string {
	if d.prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, d.prefix)
}

func (d *Dict) Lookup(ctx context.Context, _ *dict.OpSettings, key string) ([][]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, dict.ErrClosed
	}
	v, err := d.client.Get(ctx, d.key(key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get: %w", err)
	}
	return [][]byte{v}, true, nil
}

func (d *Dict) Iterate(ctx context.Context, _ *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	recurse := flags&dict.IterRecurse != 0
	exactKey := flags&dict.IterExactKey != 0
	noValue := flags&dict.IterNoValue != 0

	if exactKey {
		// Identical-to-Lookup branch wrapped in the iterator shape.
		vals, found, err := d.Lookup(ctx, nil, path)
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

	pattern := d.key(path) + "*"
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
			k := d.trim(full)
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
	// Sort flags are applied here for parity with other drivers.
	applySortFlags(rows, flags)
	return &iterator{rows: rows, idx: -1}, nil
}

func (d *Dict) Begin(_ context.Context, set *dict.OpSettings) (dict.Tx, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	ttl := time.Duration(0)
	if set != nil && set.ExpireSecs > 0 {
		ttl = time.Duration(set.ExpireSecs) * time.Second
	}
	return &tx{d: d, ttl: ttl}, nil
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
	d    *Dict
	buf  dict.MemoryTx
	ttl  time.Duration
	done bool
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

	// Pre-check that every atomic-inc target exists. Matches Dovecot's
	// dict-redis behaviour: missing key → CommitNotFound, no other ops
	// applied. Race window between EXISTS and MULTI is acceptable —
	// concurrent unset on a counter is not a yarilo-supported pattern.
	for _, op := range t.buf.Ops {
		if op.Kind != dict.OpAtomicInc {
			continue
		}
		n, err := t.d.client.Exists(ctx, t.d.key(op.Key)).Result()
		if err != nil {
			return dict.CommitFailed, fmt.Errorf("redis exists pre-check: %w", err)
		}
		if n == 0 {
			return dict.CommitNotFound, nil
		}
	}

	_, err := t.d.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, op := range t.buf.Ops {
			full := t.d.key(op.Key)
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
		// Pipeline errors during command queuing or EXEC failure.
		if errors.Is(err, redis.Nil) {
			return dict.CommitNotFound, nil
		}
		// Network errors mid-EXEC → write-uncertain per Dovecot semantics.
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

// Atomic-inc against non-integer values surfaces as a regular Commit
// error (Redis INCRBY returns "value is not an integer"). We do not
// translate that to CommitFailed-with-special-code because RFC 7162
// callers do not distinguish it from any other commit failure.
var _ = strconv.ParseInt
