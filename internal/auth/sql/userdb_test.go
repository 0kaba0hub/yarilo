package sql_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	authsql "github.com/yarilomail/yarilo/internal/auth/sql"
)

// openTestUserdb returns a fresh SQLite-backed userdb sharing its
// DSN with a passdb so the schema-creating Passdb.New runs first.
// Mirrors openTestDB in sql_test.go.
func openTestUserdb(t *testing.T, userQuery string) (*authsql.Userdb, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "users.db")
	pd, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("Passdb.New (schema seed): %v", err)
	}
	t.Cleanup(func() { pd.Close() })

	u, err := authsql.NewUserdb(authsql.Config{Driver: "sqlite", DSN: dsn, UserQuery: userQuery})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	t.Cleanup(func() { u.Close() })
	return u, dsn
}

// execSeed runs an arbitrary INSERT / ALTER against the test DB. The
// userdb test suite uses it to add columns and populate richer data
// the default schema does not carry.
func execSeed(t *testing.T, dsn, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("seed %q: %v", query, err)
	}
}

func TestUserdb_LookupDefaultSchema(t *testing.T) {
	u, dsn := openTestUserdb(t, "")
	execSeed(t, dsn,
		`INSERT INTO yarilo_users (username, password, home, mail, enabled) `+
			`VALUES ('alice', 'x', '/mail/alice', 'maildir:/mail/alice', 1)`)

	info, err := u.Lookup("alice")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil UserInfo")
	}
	if info.Username != "alice" {
		t.Errorf("Username = %q, want alice", info.Username)
	}
	if info.Home != "/mail/alice" {
		t.Errorf("Home = %q, want /mail/alice", info.Home)
	}
	if info.MailLocation != "maildir:/mail/alice" {
		t.Errorf("MailLocation = %q, want maildir:/mail/alice", info.MailLocation)
	}
}

func TestUserdb_LookupUnknownReturnsNilNil(t *testing.T) {
	u, _ := openTestUserdb(t, "")
	info, err := u.Lookup("ghost")
	if err != nil {
		t.Errorf("Lookup of unknown user: %v", err)
	}
	if info != nil {
		t.Errorf("got %+v, want nil for unknown user", info)
	}
}

func TestUserdb_LookupDisabledFiltered(t *testing.T) {
	u, dsn := openTestUserdb(t, "")
	execSeed(t, dsn, `INSERT INTO yarilo_users (username, password, enabled) VALUES ('inactive', 'x', 0)`)

	info, err := u.Lookup("inactive")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info != nil {
		t.Errorf("disabled user should not be returned, got %+v", info)
	}
}

func TestUserdb_LookupCustomQueryFullFields(t *testing.T) {
	// Operator adds rich columns and points UserQuery at their
	// own SELECT. Every typed UserInfo field that maps to a known
	// column name populates; the per-row Extras column lands in
	// UserInfo.Extra.
	dsn := filepath.Join(t.TempDir(), "rich.db")
	pd, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("Passdb.New: %v", err)
	}
	defer pd.Close()

	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN uid INTEGER DEFAULT 0`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN gid INTEGER DEFAULT 0`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN quota_rule TEXT DEFAULT ''`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN allow_nets TEXT DEFAULT ''`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN nologin INTEGER DEFAULT 0`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN custom_tier TEXT DEFAULT ''`)
	execSeed(t, dsn, `INSERT INTO yarilo_users `+
		`(username, password, home, mail, enabled, uid, gid, quota_rule, allow_nets, nologin, custom_tier) `+
		`VALUES ('alice', 'x', '/h/alice', 'maildir:/m/alice', 1, 1001, 1001, `+
		`'*:storage=5G,Trash:storage=+1G', '10.0.0.0/8, 192.168.0.0/16', 0, 'gold')`)

	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: dsn,
		UserQuery: `SELECT username, home, mail, uid, gid, quota_rule, allow_nets, nologin, custom_tier ` +
			`FROM yarilo_users WHERE username = %u AND enabled = 1`,
	})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	defer u.Close()

	info, err := u.Lookup("alice")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil UserInfo")
	}
	if info.UID != 1001 {
		t.Errorf("UID = %d, want 1001", info.UID)
	}
	if info.GID != 1001 {
		t.Errorf("GID = %d, want 1001", info.GID)
	}
	if len(info.QuotaRules) != 2 || info.QuotaRules[0] != "*:storage=5G" || info.QuotaRules[1] != "Trash:storage=+1G" {
		t.Errorf("QuotaRules = %v, want [*:storage=5G Trash:storage=+1G]", info.QuotaRules)
	}
	if len(info.AllowNets) != 2 || info.AllowNets[0] != "10.0.0.0/8" || info.AllowNets[1] != "192.168.0.0/16" {
		t.Errorf("AllowNets = %v, want [10.0.0.0/8 192.168.0.0/16]", info.AllowNets)
	}
	if info.NoLogin {
		t.Error("NoLogin should be false")
	}
	if info.Extra["custom_tier"] != "gold" {
		t.Errorf("Extra[custom_tier] = %q, want gold", info.Extra["custom_tier"])
	}
}

func TestUserdb_LookupForwardFieldsPopulateMap(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fwd.db")
	pd, _ := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	defer pd.Close()

	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN forward_origin_ip TEXT DEFAULT ''`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN forward_session TEXT DEFAULT ''`)
	execSeed(t, dsn,
		`INSERT INTO yarilo_users (username, password, enabled, forward_origin_ip, forward_session) `+
			`VALUES ('alice', 'x', 1, '192.168.1.5', 'sess-abc')`)

	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: dsn,
		UserQuery: `SELECT username, forward_origin_ip, forward_session FROM yarilo_users WHERE username = %u`,
	})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	defer u.Close()

	info, err := u.Lookup("alice")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info.Forward["origin_ip"] != "192.168.1.5" {
		t.Errorf("Forward[origin_ip] = %q, want 192.168.1.5", info.Forward["origin_ip"])
	}
	if info.Forward["session"] != "sess-abc" {
		t.Errorf("Forward[session] = %q, want sess-abc", info.Forward["session"])
	}
}

func TestUserdb_LookupBooleanColumnsAcceptVariousTruthyValues(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "bools.db")
	pd, _ := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	defer pd.Close()

	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN nologin TEXT DEFAULT ''`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN nodelay TEXT DEFAULT ''`)
	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN noauthenticate TEXT DEFAULT ''`)
	execSeed(t, dsn, `INSERT INTO yarilo_users (username, password, enabled, nologin, nodelay, noauthenticate) `+
		`VALUES ('alice', 'x', 1, 'yes', '1', 'true')`)

	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: dsn,
		UserQuery: `SELECT username, nologin, nodelay, noauthenticate FROM yarilo_users WHERE username = %u`,
	})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	defer u.Close()

	info, _ := u.Lookup("alice")
	if !info.NoLogin || !info.NoDelay || !info.NoAuthenticate {
		t.Errorf("expected all three flags true, got %+v", info)
	}
}

func TestUserdb_LookupInvalidNumericColumnReturnsError(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "badnum.db")
	pd, _ := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	defer pd.Close()

	execSeed(t, dsn, `ALTER TABLE yarilo_users ADD COLUMN uid TEXT DEFAULT ''`)
	execSeed(t, dsn,
		`INSERT INTO yarilo_users (username, password, enabled, uid) VALUES ('alice', 'x', 1, 'not-a-number')`)

	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: dsn,
		UserQuery: `SELECT username, uid FROM yarilo_users WHERE username = %u`,
	})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	defer u.Close()

	if _, err := u.Lookup("alice"); err == nil {
		t.Error("expected parse error on non-numeric uid")
	}
}

func TestUserdb_IterateRequiresQuery(t *testing.T) {
	u, _ := openTestUserdb(t, "")
	_, err := u.Iterate()
	if err == nil {
		t.Error("expected error when iterate_query is not configured")
	}
}

func TestUserdb_IterateReturnsAllEnabledUsers(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "iter.db")
	pd, _ := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	defer pd.Close()

	for _, n := range []string{"alice", "bob", "carol"} {
		execSeed(t, dsn,
			`INSERT INTO yarilo_users (username, password, enabled) VALUES ('`+n+`', 'x', 1)`)
	}
	execSeed(t, dsn, `INSERT INTO yarilo_users (username, password, enabled) VALUES ('inactive', 'x', 0)`)

	u, err := authsql.NewUserdb(authsql.Config{
		Driver: "sqlite", DSN: dsn,
		IterateQuery: `SELECT username FROM yarilo_users WHERE enabled = 1 ORDER BY username`,
	})
	if err != nil {
		t.Fatalf("NewUserdb: %v", err)
	}
	defer u.Close()

	users, err := u.Iterate()
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	want := []string{"alice", "bob", "carol"}
	if len(users) != len(want) {
		t.Fatalf("got %d users, want %d: %v", len(users), len(want), users)
	}
	for i, n := range want {
		if users[i] != n {
			t.Errorf("[%d] = %q, want %q", i, users[i], n)
		}
	}
}
