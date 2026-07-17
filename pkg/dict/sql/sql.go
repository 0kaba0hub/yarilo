// Package sql is a database/sql-backed dict driver.
//
// Supports three drivers out of the box, selected via Config.Settings:
//
//	driver: "sqlite"   — modernc.org/sqlite (pure Go, no CGO)
//	driver: "postgres" — github.com/jackc/pgx/v5/stdlib (pure Go)
//	driver: "mysql"    — github.com/go-sql-driver/mysql
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

	_ "github.com/go-sql-driver/mysql" // mysql driver
	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

func init() {
	dict.Register("sql", New)
}

// New constructs an sql dict. Recognised settings:
//
//	"driver"     string — "sqlite" | "postgres" | "mysql" (required)
//	"dsn"        string — go-database/sql DSN (required)
//	"table"      string — table name; default "dict_kv"
//	"namespace"  string — per-Dict prefix; default ""
//	"maps"       list   — enables mapped mode (see below); optional
//
// In the default (generic KV) mode the table is auto-created on Open if it does
// not exist. To opt out of auto-create (e.g. when the schema is managed by a
// migration tool), pre-create the table; the IF NOT EXISTS guard is idempotent.
//
// When "maps" is present the driver switches to mapped mode: each dict key binds
// to a table column, so a per-user row carries one column per key
// (username_field is the primary key). The operator owns the table schema, so no
// auto-create runs. Each map entry is:
//
//	{ key: "priv/quota/storage", table: quota, username_field: username, value_field: bytes }
func New(cfg dict.Config) (dict.Dict, error) {
	driver, _ := cfg.Settings["driver"].(string)
	dsn, _ := cfg.Settings["dsn"].(string)
	if driver == "" {
		return nil, errors.New("missing required setting \"driver\"")
	}
	if dsn == "" {
		return nil, errors.New("missing required setting \"dsn\"")
	}
	if driver != "sqlite" && driver != "postgres" && driver != "mysql" {
		return nil, fmt.Errorf("unknown driver %q (supported: sqlite, postgres, mysql)", driver)
	}
	table, _ := cfg.Settings["table"].(string)
	if table == "" {
		table = "dict_kv"
	}
	if !validIdent(table) {
		return nil, fmt.Errorf("table %q contains invalid characters", table)
	}
	ns, _ := cfg.Settings["namespace"].(string)

	maps, err := parseMaps(cfg.Settings["maps"])
	if err != nil {
		return nil, err
	}

	db, err := stdsql.Open(driverName(driver), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("ping: %w", err)
	}
	d := &Dict{db: db, driver: driver, table: table, ns: ns, maps: maps}
	// Mapped mode: the operator owns the schema (column types are theirs), so
	// skip auto-create. Generic KV mode manages its own table.
	if len(maps) == 0 {
		if err := d.ensureSchema(); err != nil {
			db.Close() //nolint:errcheck
			return nil, err
		}
	} else if err := d.validateMaps(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return d, nil
}

// validateMaps fails fast at Open time when a mapped table/column does not
// exist, so a typo or a forgotten CREATE TABLE surfaces at startup rather than
// silently on the first (best-effort) quota_clone write. A LIMIT 0 select never
// returns rows but still resolves the identifiers.
func (d *Dict) validateMaps() error {
	seen := make(map[string]bool, len(d.maps))
	for key, e := range d.maps {
		probe := e.table + "." + e.valCol
		if seen[probe] {
			continue
		}
		seen[probe] = true
		q := fmt.Sprintf(`SELECT %s FROM %s LIMIT 0`, e.valCol, e.table)
		if _, err := d.db.Exec(q); err != nil {
			return fmt.Errorf("mapped schema check for key %q (%s.%s): %w", key, e.table, e.valCol, err)
		}
	}
	return nil
}

// mapEntry binds a dict key to a table column: writes/reads for the key target
// value_field in table, scoped by username_field.
type mapEntry struct {
	table   string
	userCol string
	valCol  string
}

// parseMaps decodes the "maps" setting into a key→column binding table. A nil
// setting means generic KV mode (empty result). Every field is required and
// every identifier is validated so it can be interpolated into SQL safely.
func parseMaps(raw any) (map[string]mapEntry, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("setting \"maps\" must be a list, got %T", raw)
	}
	out := make(map[string]mapEntry, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("maps[%d] must be a mapping, got %T", i, item)
		}
		key, _ := m["key"].(string)
		e := mapEntry{}
		e.table, _ = m["table"].(string)
		e.userCol, _ = m["username_field"].(string)
		e.valCol, _ = m["value_field"].(string)
		if key == "" {
			return nil, fmt.Errorf("maps[%d]: missing \"key\"", i)
		}
		for name, v := range map[string]string{"table": e.table, "username_field": e.userCol, "value_field": e.valCol} {
			if v == "" {
				return nil, fmt.Errorf("maps[%d] (key %q): missing %q", i, key, name)
			}
			if !validIdent(v) {
				return nil, fmt.Errorf("maps[%d] (key %q): %s %q contains invalid characters", i, key, name, v)
			}
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("maps[%d]: duplicate key %q", i, key)
		}
		out[key] = e
	}
	return out, nil
}

func driverName(d string) string {
	switch d {
	case "postgres":
		return "pgx" // jackc/pgx/v5/stdlib registers as "pgx"
	case "sqlite":
		return "sqlite" // modernc.org/sqlite registers as "sqlite"
	case "mysql":
		return "mysql" // go-sql-driver/mysql registers as "mysql"
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
	maps   map[string]mapEntry // non-empty => mapped mode
	closed atomic.Bool
}

func (d *Dict) mapped() bool { return len(d.maps) > 0 }

func (d *Dict) Name() string                 { return "sql" }
func (d *Dict) Wait(_ context.Context) error { return nil }

// scope combines the per-Dict namespace with the per-op username so keys are
// scoped per user, matching the redis driver (which folds the username into the
// key). Empty username keeps the bare namespace, so single-user callers are
// unchanged.
func (d *Dict) scope(user string) string {
	if user == "" {
		return d.ns
	}
	if d.ns == "" {
		return user
	}
	return d.ns + "/" + user
}

// opUser returns the username from op settings, tolerating a nil set.
func opUser(set *dict.OpSettings) string {
	if set == nil {
		return ""
	}
	return set.Username
}

func (d *Dict) Close() error {
	d.closed.Store(true)
	return d.db.Close()
}

func (d *Dict) ensureSchema() error {
	// Per-driver column types. BLOB vs BYTEA: sqlite accepts BLOB, postgres
	// needs BYTEA, mysql accepts BLOB. Key columns: sqlite/postgres take TEXT in
	// a primary key, but mysql cannot index a TEXT/BLOB column without a length,
	// so it uses VARCHAR(191) (the utf8mb4 index-length limit).
	blob, keyType := "BLOB", "TEXT"
	switch d.driver {
	case "postgres":
		blob = "BYTEA"
	case "mysql":
		keyType = "VARCHAR(191)"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        namespace %s NOT NULL,
        k %s NOT NULL,
        v %s NOT NULL,
        expires BIGINT,
        PRIMARY KEY (namespace, k)
    )`, d.table, keyType, keyType, blob)
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

func (d *Dict) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, dict.ErrClosed
	}
	if d.mapped() {
		return d.lookupMapped(ctx, set, key)
	}
	q := fmt.Sprintf(`SELECT v, expires FROM %s WHERE %s`, d.table, d.argsFor("namespace", "k"))
	var v []byte
	var exp stdsql.NullInt64
	err := d.db.QueryRowContext(ctx, q, d.scope(opUser(set)), key).Scan(&v, &exp)
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

// mapFor resolves the column binding for a key, or errors if the key is not
// mapped (mapped mode matches keys exactly).
func (d *Dict) mapFor(key string) (mapEntry, error) {
	e, ok := d.maps[key]
	if !ok {
		return mapEntry{}, fmt.Errorf("key %q is not mapped", key)
	}
	return e, nil
}

// lookupMapped reads value_field from the mapped table for the op's user. A NULL
// column reads as "not found", matching the generic driver's missing-key result.
func (d *Dict) lookupMapped(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	user := opUser(set)
	if user == "" {
		return nil, false, errors.New("mapped mode requires a username")
	}
	e, err := d.mapFor(key)
	if err != nil {
		return nil, false, err
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = %s`, e.valCol, e.table, e.userCol, d.placeholder(1))
	var v stdsql.NullString
	err = d.db.QueryRowContext(ctx, q, user).Scan(&v)
	if err == stdsql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select: %w", err)
	}
	if !v.Valid {
		return nil, false, nil
	}
	return [][]byte{[]byte(v.String)}, true, nil
}

// mappedUpsert returns the driver-specific single-column upsert keyed on the
// username column, so multiple keys mapped to the same table share one row.
func (d *Dict) mappedUpsert(e mapEntry) string {
	switch d.driver {
	case "postgres":
		return fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES ($1, $2)
            ON CONFLICT (%s) DO UPDATE SET %s = EXCLUDED.%s`,
			e.table, e.userCol, e.valCol, e.userCol, e.valCol, e.valCol)
	case "mysql":
		// VALUES() in ON DUPLICATE KEY UPDATE is deprecated on MySQL 8.0.20+ but
		// works on MySQL and MariaDB alike; the alias form (AS new) is rejected
		// by MariaDB, so keep VALUES() while the primary target is MariaDB.
		return fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (?, ?)
            ON DUPLICATE KEY UPDATE %s = VALUES(%s)`,
			e.table, e.userCol, e.valCol, e.valCol, e.valCol)
	default: // sqlite
		return fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (?, ?)
            ON CONFLICT(%s) DO UPDATE SET %s = excluded.%s`,
			e.table, e.userCol, e.valCol, e.userCol, e.valCol, e.valCol)
	}
}

func (d *Dict) Iterate(ctx context.Context, set *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	if d.closed.Load() {
		return nil, dict.ErrClosed
	}
	if d.mapped() {
		return nil, errors.New("sql: Iterate is not supported in mapped mode")
	}
	ns := d.scope(opUser(set))
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
		args = []any{ns, path}
	default:
		// Use LIKE 'prefix%' for prefix iteration (path is a slash-
		// terminated string). Locally filter for shallow recursion.
		q = fmt.Sprintf(`SELECT k, v, expires FROM %s WHERE namespace = %s AND k LIKE %s`,
			d.table, d.placeholder(1), d.placeholder(2))
		args = []any{ns, path + "%"}
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
	user := ""
	if set != nil {
		if set.ExpireSecs > 0 {
			expire = time.Now().Unix() + int64(set.ExpireSecs)
		}
		user = set.Username
	}
	return &tx{d: d, sqlTx: t, expires: expire, ns: d.scope(user), user: user}, nil
}

func (d *Dict) ExpireScan(ctx context.Context) error {
	if d.closed.Load() {
		return dict.ErrClosed
	}
	if d.mapped() {
		// Mapped mode has no generic KV table and no per-row expiry column;
		// a blind DELETE would hit a non-existent (or unrelated) table.
		return errors.New("sql: ExpireScan is not supported in mapped mode")
	}
	// No namespace filter: entries are scoped per user (namespace = ns[/user]),
	// so an expired row is dropped regardless of its scope — expiry is absolute.
	q := fmt.Sprintf(`DELETE FROM %s WHERE expires IS NOT NULL AND expires <= %s`,
		d.table, d.placeholder(1))
	_, err := d.db.ExecContext(ctx, q, time.Now().Unix())
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
	ns      string // per-user scoped namespace captured at Begin (generic mode)
	user    string // raw username captured at Begin (mapped mode)
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
	case "mysql":
		return fmt.Sprintf(`INSERT INTO %s (namespace, k, v, expires) VALUES (?, ?, ?, ?)
            ON DUPLICATE KEY UPDATE v = VALUES(v), expires = VALUES(expires)`, d.table)
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

	if t.d.mapped() {
		return t.commitMapped(ctx)
	}

	for _, op := range t.buf.Ops {
		switch op.Kind {
		case dict.OpSet:
			var exp any
			if t.expires > 0 {
				exp = t.expires
			} else {
				exp = nil
			}
			if _, err := t.sqlTx.ExecContext(ctx, t.d.upsertQuery(), t.ns, op.Key, op.Value, exp); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("upsert %q: %w", op.Key, err)
			}
		case dict.OpUnset:
			q := fmt.Sprintf(`DELETE FROM %s WHERE %s`, t.d.table, t.d.argsFor("namespace", "k"))
			if _, err := t.sqlTx.ExecContext(ctx, q, t.ns, op.Key); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("delete %q: %w", op.Key, err)
			}
		case dict.OpAtomicInc:
			// Read current value under tx, validate it's an integer,
			// update with new value. Single statement would be cleaner
			// but CAST(BLOB AS BIGINT) semantics differ across drivers.
			q := fmt.Sprintf(`SELECT v FROM %s WHERE %s`, t.d.table, t.d.argsFor("namespace", "k"))
			var cur []byte
			err := t.sqlTx.QueryRowContext(ctx, q, t.ns, op.Key).Scan(&cur)
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
			upd := fmt.Sprintf(`UPDATE %s SET v = %s WHERE namespace = %s AND k = %s`,
				t.d.table, t.d.placeholder(1), t.d.placeholder(2), t.d.placeholder(3))
			if _, err := t.sqlTx.ExecContext(ctx, upd, newVal, t.ns, op.Key); err != nil {
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

// commitMapped applies buffered ops against the mapped columns. Set upserts the
// value_field for the tx user; Unset clears it to NULL (the row is shared by
// other mapped columns, so it is never deleted here); AtomicInc is unsupported.
func (t *tx) commitMapped(ctx context.Context) (dict.CommitResult, error) {
	if t.user == "" {
		t.sqlTx.Rollback() //nolint:errcheck
		return dict.CommitFailed, errors.New("mapped mode requires a username")
	}
	if t.expires > 0 {
		// Mapped columns carry no per-row expiry, so a TTL cannot be honoured or
		// reaped; fail loudly rather than silently keep rows forever.
		t.sqlTx.Rollback() //nolint:errcheck
		return dict.CommitFailed, errors.New("sql: per-key TTL (expire_secs) is not supported in mapped mode")
	}
	for _, op := range t.buf.Ops {
		e, err := t.d.mapFor(op.Key)
		if err != nil {
			t.sqlTx.Rollback() //nolint:errcheck
			return dict.CommitFailed, err
		}
		switch op.Kind {
		case dict.OpSet:
			// Bind as string, not []byte: pgx encodes []byte as bytea, which a
			// numeric target column (BIGINT) rejects. Text binds coerce cleanly
			// on all three drivers.
			if _, err := t.sqlTx.ExecContext(ctx, t.d.mappedUpsert(e), t.user, string(op.Value)); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("upsert %q: %w", op.Key, err)
			}
		case dict.OpUnset:
			q := fmt.Sprintf(`UPDATE %s SET %s = NULL WHERE %s = %s`,
				e.table, e.valCol, e.userCol, t.d.placeholder(1))
			if _, err := t.sqlTx.ExecContext(ctx, q, t.user); err != nil {
				t.sqlTx.Rollback() //nolint:errcheck
				return dict.CommitFailed, fmt.Errorf("unset %q: %w", op.Key, err)
			}
		case dict.OpAtomicInc:
			t.sqlTx.Rollback() //nolint:errcheck
			return dict.CommitFailed, errors.New("sql: AtomicInc is not supported in mapped mode")
		}
	}
	if err := t.sqlTx.Commit(); err != nil {
		return dict.CommitFailed, fmt.Errorf("commit tx: %w", err)
	}
	return dict.CommitOK, nil
}
