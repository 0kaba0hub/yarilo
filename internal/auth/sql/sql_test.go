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
	p, err := authsql.New("sqlite", dsn)
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
		name       string
		username   string
		password   string
		wantNil    bool
		wantResult protocol.AuthResult
		wantUser   string
	}{
		{
			name:       "valid user correct password",
			username:   "alice",
			password:   "secret",
			wantNil:    false,
			wantResult: protocol.AuthOK,
			wantUser:   "alice",
		},
		{
			name:       "valid user wrong password",
			username:   "alice",
			password:   "wrong",
			wantNil:    false,
			wantResult: protocol.AuthFail,
		},
		{
			name:     "unknown user returns nil",
			username: "nobody",
			password: "pass",
			wantNil:  true,
		},
		{
			name:       "disabled user",
			username:   "disabled",
			password:   "anypass",
			wantNil:    false,
			wantResult: protocol.AuthFail,
		},
		{
			name:       "PLAIN prefix stored password correct",
			username:   "bob",
			password:   "p@ssw0rd",
			wantNil:    false,
			wantResult: protocol.AuthOK,
			wantUser:   "bob",
		},
		{
			name:       "plain stored password no prefix correct",
			username:   "charlie",
			password:   "plainpass",
			wantNil:    false,
			wantResult: protocol.AuthOK,
			wantUser:   "charlie",
		},
		{
			name:       "bcrypt stored correct",
			username:   "dave",
			password:   "bcryptpass",
			wantNil:    false,
			wantResult: protocol.AuthOK,
			wantUser:   "dave",
		},
		{
			name:       "bcrypt stored wrong",
			username:   "dave",
			password:   "wrong",
			wantNil:    false,
			wantResult: protocol.AuthFail,
		},
		{
			name:       "sha512-crypt stored correct",
			username:   "eve",
			password:   "shapass",
			wantNil:    false,
			wantResult: protocol.AuthOK,
			wantUser:   "eve",
		},
		{
			name:       "sha512-crypt stored wrong",
			username:   "eve",
			password:   "wrong",
			wantNil:    false,
			wantResult: protocol.AuthFail,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := p.Authenticate(tc.username, tc.password, "imap")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil {
				if resp != nil {
					t.Fatalf("expected nil response, got %+v", resp)
				}
				return
			}
			if resp == nil {
				t.Fatal("expected non-nil response, got nil")
			}
			if resp.Result != tc.wantResult {
				t.Fatalf("result: got %v, want %v", resp.Result, tc.wantResult)
			}
			if tc.wantUser != "" && resp.Username != tc.wantUser {
				t.Fatalf("username: got %q, want %q", resp.Username, tc.wantUser)
			}
		})
	}
}

func TestNew_InvalidDSN(t *testing.T) {
	_, err := authsql.New("sqlite", "/nonexistent/path/to/users.db")
	if err == nil {
		t.Fatal("expected error for bad DSN, got nil")
	}
}

func TestClose(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "close.db")
	p, err := authsql.New("sqlite", dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNew_UnsupportedDriver(t *testing.T) {
	if _, err := authsql.New("oracle", "ignored"); err == nil {
		t.Fatal("expected error for unsupported driver, got nil")
	}
}

// runDriverSmoke exercises end-to-end CREATE TABLE + INSERT + Authenticate
// against a real MySQL/Postgres server. Requires an env-var DSN — skipped
// otherwise so CI without DB credentials still passes.
func runDriverSmoke(t *testing.T, driver, dsn, insertParam string) {
	t.Helper()
	p, err := authsql.New(driver, dsn)
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

	resp, err := p.Authenticate("smoketest@example.com", "smokepass", "imap")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		t.Fatalf("expected AuthOK, got %+v", resp)
	}

	resp, _ = p.Authenticate("smoketest@example.com", "wrong", "imap")
	if resp == nil || resp.Result != protocol.AuthFail {
		t.Fatalf("expected AuthFail for wrong password, got %+v", resp)
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
