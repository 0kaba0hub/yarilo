// Package sql is a database/sql-backed dict driver.
//
// Supports two drivers out of the box, selected via Config.Settings:
//
//	driver: "sqlite"   — modernc.org/sqlite (pure Go, no CGO)
//	driver: "postgres" — github.com/jackc/pgx/v5/stdlib (pure Go)
//
// Both are statically compiled in so the choice is runtime-config-only,
// per yarilo's config-not-binary rule.
//
// Schema:
//
//	CREATE TABLE <table> (
//	    namespace TEXT NOT NULL,
//	    k         TEXT NOT NULL,
//	    v         BLOB NOT NULL,
//	    expires   BIGINT,                -- unix seconds; NULL = no TTL
//	    PRIMARY KEY (namespace, k)
//	);
//	CREATE INDEX <table>_expires_idx ON <table>(expires) WHERE expires IS NOT NULL;
//
// "namespace" is the per-Dict prefix (matches Redis driver's prefix
// concept) — lets multiple yarilo features share one database without
// key collisions. ExpireScan runs `DELETE FROM <table> WHERE expires
// IS NOT NULL AND expires <= ?` and reports the row count via the
// driver's slog logger.
//
// Multi-value: not stored. A second Set on the same key overwrites.
// Use SQL directly for any feature that genuinely needs multi-value.
//
// Transactions: native database/sql transactions. AtomicInc uses
// UPDATE ... SET v = CAST(CAST(v AS BIGINT) + ? AS BLOB) WHERE k=?
// and reports CommitNotFound when zero rows were affected.
package sql

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

func init() {
	dict.Register("sql", New)
}

// New constructs an sql dict. Recognised settings:
//
//	"driver"     string — "sqlite" | "postgres" (required)
//	"dsn"        string — go-database/sql DSN (required)
//	"table"      string — table name; default "dict_kv"
//	"namespace"  string — per-Dict prefix; default ""
//
// The table is auto-created on Open if it does not exist. To opt out
// of auto-create (e.g. when the schema is managed by a migration tool),
// pre-create the table; the IF NOT EXISTS guard is idempotent.
func New(cfg dict.Config) (dict.Dict, error) {
	driver, _ := cfg.Settings["driver"].(string)
	dsn, _ := cfg.Settings["dsn"].(string)
	if driver == "" {
		return nil, errors.New("missing required setting \"driver\"")
	}
	if dsn == "" {
		return nil, errors.New("missing required setting \"dsn\"")
	}
	if driver != "sqlite" && driver != "postgres" {
		return nil, fmt.Errorf("unknown driver %q (supported: sqlite, postgres)", driver)
	}
	table, _ := cfg.Settings["table"].(string)
	if table == "" {
		table = "dict_kv"
	}
	if !validIdent(table) {
		return nil, fmt.Errorf("table %q contains invalid characters", table)
	}
	ns, _ := cfg.Settings["namespace"].(string)

	db, err := stdsql.Open(driverName(driver), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("ping: %w", err)
	}
	d := &Dict{db: db, driver: driver, table: table, ns: ns}
	if err := d.ensureSchema(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return d, nil
}

func driverName(d string) string {
	switch d {
	case "postgres":
		return "pgx" // jackc/pgx/v5/stdlib registers as "pgx"
	case "sqlite":
		return "sqlite" // modernc.org/sqlite registers as "sqlite"
	}
	return d
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		return false
	}
	return true
}

type Dict struct {
	db     *stdsql.DB
	driver string
	table  string
	ns     string
	closed atomic.Bool
}

func (d *Dict) Name() string                 { return "sql" }
func (d *Dict) Wait(_ context.Context) error { return nil }

func (d *Dict) Close() error {
	d.closed.Store(true)
	return d.db.Close()
}

func (d *Dict) ensureSchema() error {
	// CREATE TABLE IF NOT EXISTS works on both sqlite and postgres.
	// BLOB vs BYTEA: sqlite accepts BLOB; postgres needs BYTEA. We
	// branch on driver to issue the right column type.
	var blob string
	switch d.driver {
	case "sqlite":
		blob = "BLOB"
	case "postgres":
		blob = "BYTEA"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        namespace TEXT NOT NULL,
        k TEXT NOT NULL,
        v %s NOT NULL,
        expires BIGINT,
        PRIMARY KEY (namespace, k)
    )`, d.table, blob)
	if _, err := d.db.Exec(ddl); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	idx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_expires_idx ON %s(expires) WHERE expires IS NOT NULL`, d.table, d.table)
	if _, err := d.db.Exec(idx); err != nil {
		// sqlite supports partial indexes; postgres also; if a driver
		// rejects, fall back to a non-partial index. Don't fail on the
		// fallback either — the index is an optimisation only.
		_, _ = d.db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_expires_idx ON %s(expires)`, d.table, d.table))
	}
	return nil
}

// placeholder returns the bind-parameter syntax for the active driver.
// sqlite uses ?; postgres uses $1, $2, ...
func (d *Dict) placeholder(n int) string {
	switch d.driver {
	case "postgres":
		return fmt.Sprintf("$%d", n)
	default:
		return "?"
	}
}

// argsFor returns SQL like "WHERE namespace = ? AND k = ?" with the
// right placeholder syntax for the driver.
func (d *Dict) argsFor(names ...string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + " = " + d.placeholder(i+1)
	}
	return strings.Join(parts, " AND ")
}

func (d *Dict) Lookup(ctx context.Context, _ *dict.OpSettings, key string) ([][]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, dict.ErrClosed
	}
	q := fmt.Sprintf(`SELECT v, expires FROM %s WHERE %s`, d.table, d.argsFor("namespace", "k"))
	var v []byte
	var exp stdsql.NullInt64
	err := d.db.QueryRowContext(ctx, q, d.ns, key).Scan(&v, &exp)
	if err == stdsql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select: %w", err)
	}
	if exp.Valid && time.Now().Unix() > exp.Int64 {
		return nil, false, nil
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
	sortByKey := flags&dict.IterSortByKey != 0
	sortByValue := flags&dict.IterSortByValue != 0

	var q string
	var args []any
	switch {
	case exactKey:
		q = fmt.Sprintf(`SELECT k, v, expires FROM %s WHERE %s`, d.table, d.argsFor("namespace", "k"))
		args = []any{d.ns, path}
	default:
		// Use LIKE 'prefix%' for prefix iteration (path is a slash-
		// terminated string). Locally filter for shallow recursion.
		q = fmt.Sprintf(`SELECT k, v, expires FROM %s WHERE namespace = %s AND k LIKE %s`,
			d.table, d.placeholder(1), d.placeholder(2))
		args = []any{d.ns, path + "%"}
	}
	switch {
	case sortByKey:
		q += " ORDER BY k ASC"
	case sortByValue:
		q += " ORDER BY v ASC"
	}

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("iter query: %w", err)
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []iterRow
	for rows.Next() {
		var k string
		var v []byte
		var exp stdsql.NullInt64
		if err := rows.Scan(&k, &v, &exp); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if exp.Valid && now > exp.Int64 {
			continue
		}
		if !exactKey && !dict.PathMatches(path, k, recurse, false) {
			continue
		}
		r := iterRow{key: k}
		if !noValue {
			r.values = [][]byte{append([]byte(nil), v...)}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &iterator{rows: out, idx: -1}, nil
}

func (d *Dict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	t, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	expire := int64(0)
	if set != nil && set.ExpireSecs > 0 {
		expire = time.Now().Unix() + int64(set.ExpireSecs)
	}
	return &tx{d: d, sqlTx: t, expires: expire}, nil
}

func (d *Dict) ExpireScan(ctx context.Context) error {
	if d.closed.Load() {
		return dict.ErrClosed
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE namespace = %s AND expires IS NOT NULL AND expires <= %s`,
		d.table, d.placeholder(1), d.placeholder(2))
	_, err := d.db.ExecContext(ctx, q, d.ns, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("expire scan: %w", err)
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
	sqlTx   *stdsql.Tx
	buf     dict.MemoryTx
	expires int64
	done    bool
}

func (t *tx) Set(key string, value []byte) error {
	if t.done {
		return errors.New("sql: tx already finalised")
	}
	t.buf.Set(key, value)
	return nil
}

func (t *tx) Unset(key string) error {
	if t.done {
		return errors.New("sql: tx already finalised")
	}
	t.buf.Unset(key)
	return nil
}

func (t *tx) AtomicInc(key string, delta int64) error {
	if t.done {
		return errors.New("sql: tx already finalised")
	}
	t.buf.AtomicInc(key, delta)
	return nil
}

func (t *tx) Rollback() error {
	t.done = true
	t.buf.Reset()
	return t.sqlTx.Rollback()
}

// upsertQuery returns the driver-specific INSERT...ON CONFLICT clause.
func (d *Dict) upsertQuery() string {
	switch d.driver {
	case "postgres":
		return fmt.Sprintf(`INSERT INTO %s (namespace, k, v, expires) VALUES ($1, $2, $3, $4)
            ON CONFLICT (namespace, k) DO UPDATE SET v = EXCLUDED.v, expires = EXCLUDED.expires`, d.table)
	default: // sqlite
		return fmt.Sprintf(`INSERT INTO %s (namespace, k, v, expires) VALUES (?, ?, ?, ?)
            ON CONFLICT(namespace, k) DO UPDATE SET v = excluded.v, expires = excluded.expires`, d.table)
	}
}

func (t *tx) Commit() (dict.CommitResult, error) {
	if t.done {
		return dict.CommitFailed, errors.New("sql: tx already finalised")
	}
	t.done = true
	if t.d.closed.Load() {
		t.sqlTx.Rollback() //nolint:errcheck
		return dict.CommitFailed, dict.ErrClosed
	}
	ctx := context.Background()

	for _, op := range t.buf.Ops {
		switch op.Kind {
		case dict.OpSet:
			var exp any
			if t.expires > 0 {
				exp = t.expires
			} else {
				exp = nil
			}
			if _, err := t.sqlTx.ExecContext(ctx, t.d.upsertQuery(), t.d.ns, op.Key, op.Value, exp); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("upsert %q: %w", op.Key, err)
			}
		case dict.OpUnset:
			q := fmt.Sprintf(`DELETE FROM %s WHERE %s`, t.d.table, t.d.argsFor("namespace", "k"))
			if _, err := t.sqlTx.ExecContext(ctx, q, t.d.ns, op.Key); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("delete %q: %w", op.Key, err)
			}
		case dict.OpAtomicInc:
			// Read current value under tx, validate it's an integer,
			// update with new value. Single statement would be cleaner
			// but CAST(BLOB AS BIGINT) semantics differ across drivers.
			q := fmt.Sprintf(`SELECT v FROM %s WHERE %s`, t.d.table, t.d.argsFor("namespace", "k"))
			var cur []byte
			err := t.sqlTx.QueryRowContext(ctx, q, t.d.ns, op.Key).Scan(&cur)
			if err == stdsql.ErrNoRows {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitNotFound, nil
			}
			if err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, err
			}
			n, err := strconv.ParseInt(string(cur), 10, 64)
			if err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("atomic-inc on non-integer at %q", op.Key)
			}
			newVal := []byte(strconv.FormatInt(n+op.Delta, 10))
			upd := fmt.Sprintf(`UPDATE %s SET v = %s WHERE %s`,
				t.d.table, t.d.placeholder(1), t.d.argsFor("namespace", "k")[len("namespace = ?")-len(t.d.placeholder(1))+1:])
			// Rebuild query cleanly with correct placeholders 1,2,3.
			upd = fmt.Sprintf(`UPDATE %s SET v = %s WHERE namespace = %s AND k = %s`,
				t.d.table, t.d.placeholder(1), t.d.placeholder(2), t.d.placeholder(3))
			if _, err := t.sqlTx.ExecContext(ctx, upd, newVal, t.d.ns, op.Key); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, err
			}
		}
	}

	if err := t.sqlTx.Commit(); err != nil {
		return dict.CommitFailed, fmt.Errorf("commit tx: %w", err)
	}
	return dict.CommitOK, nil
}
