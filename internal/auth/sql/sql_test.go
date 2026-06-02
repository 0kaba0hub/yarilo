package sql_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"golang.org/x/crypto/bcrypt"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
)

func openTestDB(t *testing.T) (*authsql.Passdb, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "users.db")
	p, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p, dsn
}

func insertUser(t *testing.T, dsn, username, password string, enabled int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("insertUser open: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO yarilo_users (username, password, home, mail, enabled) VALUES (?, ?, '', '', ?)`,
		username, password, enabled,
	)
	if err != nil {
		t.Fatalf("insertUser exec: %v", err)
	}
}

func TestAuthenticate(t *testing.T) {
	p, dsn := openTestDB(t)

	bcryptHash, err := bcrypt.GenerateFromPassword([]byte("bcryptpass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	shaHash, err := sha512_crypt.New().Generate([]byte("shapass"), []byte("$6$NaClNaCl"))
	if err != nil {
		t.Fatalf("sha512_crypt: %v", err)
	}

	insertUser(t, dsn, "alice", "secret", 1)
	insertUser(t, dsn, "bob", "{PLAIN}p@ssw0rd", 1)
	insertUser(t, dsn, "charlie", "plainpass", 1)
	insertUser(t, dsn, "disabled", "anypass", 0)
	insertUser(t, dsn, "dave", "{BCRYPT}"+string(bcryptHash), 1)
	insertUser(t, dsn, "eve", "{SHA512-CRYPT}"+shaHash, 1)

	cases := []struct {
		name     string
		username string
		password string
		want     protocol.Result
		wantUser string
	}{
		{name: "valid user correct password", username: "alice", password: "secret", want: protocol.ResultOK, wantUser: "alice"},
		{name: "valid user wrong password", username: "alice", password: "wrong", want: protocol.ResultFail},
		{name: "unknown user returns Next", username: "nobody", password: "pass", want: protocol.ResultNext},
		{name: "disabled user", username: "disabled", password: "anypass", want: protocol.ResultFail},
		{name: "PLAIN prefix stored password correct", username: "bob", password: "p@ssw0rd", want: protocol.ResultOK, wantUser: "bob"},
		{name: "plain stored password no prefix correct", username: "charlie", password: "plainpass", want: protocol.ResultOK, wantUser: "charlie"},
		{name: "bcrypt stored correct", username: "dave", password: "bcryptpass", want: protocol.ResultOK, wantUser: "dave"},
		{name: "bcrypt stored wrong", username: "dave", password: "wrong", want: protocol.ResultFail},
		{name: "sha512-crypt stored correct", username: "eve", password: "shapass", want: protocol.ResultOK, wantUser: "eve"},
		{name: "sha512-crypt stored wrong", username: "eve", password: "wrong", want: protocol.ResultFail},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &protocol.Request{
				Username: tc.username,
				Password: tc.password,
				Service:  "imap",
				Fields:   protocol.NewFields(),
			}
			got, err := p.Authenticate(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("result: got %v, want %v", got, tc.want)
			}
			if tc.wantUser != "" {
				v, ok := req.Fields.Get("user")
				if !ok || v != tc.wantUser {
					t.Fatalf("Fields[user] = %q,%v; want %q,true", v, ok, tc.wantUser)
				}
			}
		})
	}
}

// TestAuthenticate_PopulatesFieldsBag verifies the Phase AUTH-2
// PR 1 wire-prep work: the SQL passdb now writes its result into
// AuthResponse.Fields in parallel with the typed Home / MailLoc
// members. handleAuth's buildAuthOK prefers the bag when present,
// so this assertion is the regression guard for the new wire path.
func TestAuthenticate_PopulatesFieldsBag(t *testing.T) {
	p, dsn := openTestDB(t)
	insertUser(t, dsn, "alice", "secret", 1)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(
		`UPDATE yarilo_users SET home = ?, mail = ? WHERE username = ?`,
		"/h/alice", "maildir:/m/alice", "alice")
	if err != nil {
		t.Fatalf("update mail/home: %v", err)
	}

	req := &protocol.Request{
		Username: "alice",
		Password: "secret",
		Fields:   protocol.NewFields(),
	}
	got, err := p.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != protocol.ResultOK {
		t.Fatalf("Result = %v, want ResultOK", got)
	}
	for _, kv := range []struct{ k, want string }{
		{"user", "alice"},
		{"home", "/h/alice"},
		{"mail", "maildir:/m/alice"},
	} {
		v, ok := req.Fields.Get(kv.k)
		if !ok || v != kv.want {
			t.Errorf("Fields[%q] = %q,%v; want %q,true", kv.k, v, ok, kv.want)
		}
	}
}

func TestNew_InvalidDSN(t *testing.T) {
	_, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: "/nonexistent/path/to/users.db"})
	if err == nil {
		t.Fatal("expected error for bad DSN, got nil")
	}
}

func TestClose(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "close.db")
	p, err := authsql.New(authsql.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNew_UnsupportedDriver(t *testing.T) {
	if _, err := authsql.New(authsql.Config{Driver: "oracle", DSN: "ignored"}); err == nil {
		t.Fatal("expected error for unsupported driver, got nil")
	}
}

// runDriverSmoke exercises end-to-end CREATE TABLE + INSERT + Authenticate
// against a real MySQL/Postgres server. Requires an env-var DSN — skipped
// otherwise so CI without DB credentials still passes.
func runDriverSmoke(t *testing.T, driver, dsn, insertParam string) {
	t.Helper()
	p, err := authsql.New(authsql.Config{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatalf("New(%s): %v", driver, err)
	}
	t.Cleanup(func() { p.Close() })

	db, err := sql.Open(driverRegistered(driver), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// fresh slate
	if _, err := db.Exec(`DELETE FROM yarilo_users WHERE username = 'smoketest@example.com'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte("smokepass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	stored := "{BCRYPT}" + string(pwHash)

	stmt := `INSERT INTO yarilo_users (username, password, home, mail, enabled) VALUES (` +
		insertParam + `, ` + insertParam + `, '', '', 1)`
	if _, err := db.Exec(stmt, "smoketest@example.com", stored); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM yarilo_users WHERE username = 'smoketest@example.com'`)
	})

	req := &protocol.Request{
		Username: "smoketest@example.com",
		Password: "smokepass",
		Service:  "imap",
		Fields:   protocol.NewFields(),
	}
	got, err := p.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != protocol.ResultOK {
		t.Fatalf("expected ResultOK, got %v", got)
	}

	bad := &protocol.Request{
		Username: "smoketest@example.com",
		Password: "wrong",
		Service:  "imap",
		Fields:   protocol.NewFields(),
	}
	got, _ = p.Authenticate(bad)
	if got != protocol.ResultFail {
		t.Fatalf("expected ResultFail for wrong password, got %v", got)
	}
}

func driverRegistered(driver string) string {
	if driver == "postgres" {
		return "pgx"
	}
	return driver
}

func TestMySQL_Smoke(t *testing.T) {
	dsn := os.Getenv("YARILO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("YARILO_TEST_MYSQL_DSN not set")
	}
	runDriverSmoke(t, "mysql", dsn, "?")
}

func TestPostgres_Smoke(t *testing.T) {
	dsn := os.Getenv("YARILO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("YARILO_TEST_POSTGRES_DSN not set")
	}
	runDriverSmoke(t, "postgres", dsn, "$1")
}
