package sql

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/dict/dicttest"
)

func sqliteFactory(t *testing.T) dict.Dict {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dict.sqlite")
	d, err := New(dict.Config{Settings: map[string]any{
		"driver": "sqlite",
		"dsn":    dbPath,
	}})
	if err != nil {
		t.Fatalf("new sqlite dict: %v", err)
	}
	return d
}

func TestContractSuite_SQLite(t *testing.T) {
	dicttest.RunSuite(t, sqliteFactory)
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist.sqlite")
	d1, _ := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath}})
	tx, _ := d1.Begin(context.Background(), nil)
	_ = tx.Set("k", []byte("v"))
	_, _ = tx.Commit()
	d1.Close() //nolint:errcheck

	d2, _ := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath}})
	defer d2.Close() //nolint:errcheck
	vals, found, err := d2.Lookup(context.Background(), nil, "k")
	if err != nil || !found || string(vals[0]) != "v" {
		t.Fatalf("post-reopen lookup: err=%v found=%v vals=%q", err, found, vals)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ns.sqlite")
	a, _ := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath, "namespace": "ns-a"}})
	b, _ := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath, "namespace": "ns-b"}})
	defer a.Close()
	defer b.Close()

	tx, _ := a.Begin(context.Background(), nil)
	_ = tx.Set("k", []byte("a"))
	_, _ = tx.Commit()

	_, foundB, _ := b.Lookup(context.Background(), nil, "k")
	if foundB {
		t.Error("namespace isolation broken: ns-b sees ns-a's keys")
	}
}

func TestExpireScanPersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "expire.sqlite")
	d, _ := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath}})
	defer d.Close()
	tx, _ := d.Begin(context.Background(), &dict.OpSettings{ExpireSecs: 1})
	_ = tx.Set("ephemeral", []byte("v"))
	_, _ = tx.Commit()

	// Hand-rotate the row's expiry to a past unix time so ExpireScan deletes it.
	_, err := d.(*Dict).db.Exec(`UPDATE dict_kv SET expires = 1 WHERE k = ?`, "ephemeral")
	if err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if err := d.ExpireScan(context.Background()); err != nil {
		t.Fatalf("expire scan: %v", err)
	}
	_, found, _ := d.Lookup(context.Background(), nil, "ephemeral")
	if found {
		t.Error("expired row not deleted by ExpireScan")
	}
}

func TestMissingDriverErrors(t *testing.T) {
	if _, err := New(dict.Config{}); err == nil {
		t.Error("missing driver should error")
	}
	if _, err := New(dict.Config{Settings: map[string]any{"driver": "no-such"}}); err == nil {
		t.Error("unknown driver should error")
	}
	if _, err := New(dict.Config{Settings: map[string]any{"driver": "sqlite"}}); err == nil {
		t.Error("missing dsn should error")
	}
}

// TestMySQLDriverAccepted verifies mysql is a recognised driver: New must reach
// the connection stage (ping error against a bogus DSN), not reject the driver
// name. No live server is contacted.
func TestMySQLDriverAccepted(t *testing.T) {
	_, err := New(dict.Config{Settings: map[string]any{
		"driver": "mysql",
		"dsn":    "yarilo:bad@tcp(127.0.0.1:0)/nodb",
	}})
	if err == nil {
		t.Fatal("expected a connection error against a bogus DSN")
	}
	if strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("mysql should be a recognised driver, got: %v", err)
	}
}

func TestInvalidTableNameRejected(t *testing.T) {
	_, err := New(dict.Config{Settings: map[string]any{
		"driver": "sqlite",
		"dsn":    filepath.Join(t.TempDir(), "x.sqlite"),
		"table":  "drop; --",
	}})
	if err == nil {
		t.Error("invalid table identifier should be rejected (sql-injection guard)")
	}
}

func TestRegisteredAtInit(t *testing.T) {
	for _, n := range dict.Drivers() {
		if n == "sql" {
			return
		}
	}
	t.Errorf("sql driver not registered: %v", dict.Drivers())
}

// TestPerUserScoping verifies OpSettings.Username scopes keys so two users do
// not collide on the same rows (regression for the quota_clone SQL target).
func TestPerUserScoping(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "scope.db")
	d, err := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": dbPath}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	ctx := context.Background()

	set := func(user, key, val string) {
		tx, err := d.Begin(ctx, &dict.OpSettings{Username: user})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Set(key, []byte(val)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	get := func(user, key string) string {
		vs, found, err := d.Lookup(ctx, &dict.OpSettings{Username: user}, key)
		if err != nil || !found || len(vs) == 0 {
			t.Fatalf("lookup %s/%s: found=%v err=%v", user, key, found, err)
		}
		return string(vs[0])
	}

	const key = "priv/quota/storage"
	set("u1@x", key, "111")
	set("u2@x", key, "222") // must NOT overwrite u1's row

	if got := get("u1@x", key); got != "111" {
		t.Errorf("u1 = %q, want 111 (collision with u2?)", got)
	}
	if got := get("u2@x", key); got != "222" {
		t.Errorf("u2 = %q, want 222", got)
	}
	// A different user's key is absent, not the other user's value.
	if _, found, _ := d.Lookup(ctx, &dict.OpSettings{Username: "ghost@x"}, key); found {
		t.Errorf("ghost should have no value")
	}
}

// mappedDict opens a sqlite dict in mapped mode and pre-creates the operator-
// owned quota(username, bytes, messages) table, matching the Dovecot-compatible
// schema from #590.
func mappedDict(t *testing.T) *Dict {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mapped.sqlite")
	d, err := New(dict.Config{Settings: map[string]any{
		"driver": "sqlite",
		"dsn":    dbPath,
		"maps": []any{
			map[string]any{"key": "priv/quota/storage", "table": "quota", "username_field": "username", "value_field": "bytes"},
			map[string]any{"key": "priv/quota/messages", "table": "quota", "username_field": "username", "value_field": "messages"},
		},
	}})
	if err != nil {
		t.Fatalf("new mapped dict: %v", err)
	}
	sd := d.(*Dict)
	if _, err := sd.db.Exec(`CREATE TABLE quota (username TEXT PRIMARY KEY, bytes BIGINT, messages BIGINT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return sd
}

func mappedSet(t *testing.T, d *Dict, user, key, val string) {
	t.Helper()
	tx, err := d.Begin(context.Background(), &dict.OpSettings{Username: user})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Set(key, []byte(val)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMappedModeColumns verifies two keys mapped to the same table share one row
// (one column each), and that Lookup reads the mapped columns back.
func TestMappedModeColumns(t *testing.T) {
	d := mappedDict(t)
	defer d.Close() //nolint:errcheck
	ctx := context.Background()

	mappedSet(t, d, "u1@x", "priv/quota/storage", "860809")
	mappedSet(t, d, "u1@x", "priv/quota/messages", "13")

	// Both keys land in the same physical row.
	var rows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM quota WHERE username = ?`, "u1@x").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("expected 1 shared row, got %d", rows)
	}

	get := func(key string) (string, bool) {
		vs, found, err := d.Lookup(ctx, &dict.OpSettings{Username: "u1@x"}, key)
		if err != nil {
			t.Fatalf("lookup %s: %v", key, err)
		}
		if !found {
			return "", false
		}
		return string(vs[0]), true
	}
	if v, _ := get("priv/quota/storage"); v != "860809" {
		t.Errorf("storage = %q, want 860809", v)
	}
	if v, _ := get("priv/quota/messages"); v != "13" {
		t.Errorf("messages = %q, want 13", v)
	}
	// Another user has no row.
	if _, found := get2(t, d, "ghost@x", "priv/quota/storage"); found {
		t.Error("ghost should have no mapped value")
	}
}

func get2(t *testing.T, d *Dict, user, key string) (string, bool) {
	t.Helper()
	vs, found, err := d.Lookup(context.Background(), &dict.OpSettings{Username: user}, key)
	if err != nil {
		t.Fatalf("lookup %s/%s: %v", user, key, err)
	}
	if !found {
		return "", false
	}
	return string(vs[0]), true
}

// TestMappedUnsetNullsColumn verifies Unset clears only the mapped column to
// NULL, leaving the sibling column (and the row) intact.
func TestMappedUnsetNullsColumn(t *testing.T) {
	d := mappedDict(t)
	defer d.Close() //nolint:errcheck

	mappedSet(t, d, "u1@x", "priv/quota/storage", "500")
	mappedSet(t, d, "u1@x", "priv/quota/messages", "7")

	tx, _ := d.Begin(context.Background(), &dict.OpSettings{Username: "u1@x"})
	if err := tx.Unset("priv/quota/storage"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit unset: %v", err)
	}

	if _, found := get2(t, d, "u1@x", "priv/quota/storage"); found {
		t.Error("storage should read as not-found after Unset (NULL)")
	}
	if v, found := get2(t, d, "u1@x", "priv/quota/messages"); !found || v != "7" {
		t.Errorf("messages = %q found=%v, want 7 (sibling column intact)", v, found)
	}
	// The row is not deleted, just the column nulled.
	var rows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM quota WHERE username = ?`, "u1@x").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("row count = %d, want 1 (row must survive column Unset)", rows)
	}
}

func TestMappedModeErrors(t *testing.T) {
	d := mappedDict(t)
	defer d.Close() //nolint:errcheck
	ctx := context.Background()

	// Unknown key is rejected (exact match only).
	if _, _, err := d.Lookup(ctx, &dict.OpSettings{Username: "u1@x"}, "priv/unknown"); err == nil {
		t.Error("unknown key lookup should error in mapped mode")
	}
	// Missing username is rejected.
	if _, _, err := d.Lookup(ctx, &dict.OpSettings{}, "priv/quota/storage"); err == nil {
		t.Error("empty username lookup should error in mapped mode")
	}
	// AtomicInc is unsupported.
	tx, _ := d.Begin(ctx, &dict.OpSettings{Username: "u1@x"})
	_ = tx.AtomicInc("priv/quota/storage", 1)
	if res, err := tx.Commit(); err == nil || res == dict.CommitOK {
		t.Errorf("AtomicInc should fail in mapped mode: res=%v err=%v", res, err)
	}
	// Iterate is unsupported.
	if _, err := d.Iterate(ctx, &dict.OpSettings{Username: "u1@x"}, "priv/", 0); err == nil {
		t.Error("Iterate should error in mapped mode")
	}
}

func TestParseMapsValidation(t *testing.T) {
	cases := []struct {
		name string
		maps any
	}{
		{"not a list", "nope"},
		{"item not a mapping", []any{"nope"}},
		{"missing key", []any{map[string]any{"table": "quota", "username_field": "username", "value_field": "bytes"}}},
		{"missing value_field", []any{map[string]any{"key": "k", "table": "quota", "username_field": "username"}}},
		{"invalid table ident", []any{map[string]any{"key": "k", "table": "quota; --", "username_field": "username", "value_field": "bytes"}}},
		{"duplicate key", []any{
			map[string]any{"key": "k", "table": "quota", "username_field": "username", "value_field": "bytes"},
			map[string]any{"key": "k", "table": "quota", "username_field": "username", "value_field": "messages"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseMaps(c.maps); err == nil {
				t.Errorf("expected parseMaps to reject %s", c.name)
			}
		})
	}
	// nil => generic KV mode, no error, no maps.
	if m, err := parseMaps(nil); err != nil || m != nil {
		t.Errorf("nil maps: m=%v err=%v, want nil,nil", m, err)
	}
}
