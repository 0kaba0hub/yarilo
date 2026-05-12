package sql_test

import (
	"database/sql"
	"path/filepath"
	"testing"

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

	insertUser(t, dsn, "alice", "secret", 1)
	insertUser(t, dsn, "bob", "{PLAIN}p@ssw0rd", 1)
	insertUser(t, dsn, "charlie", "plainpass", 1)
	insertUser(t, dsn, "disabled", "anypass", 0)

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
