package sql_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	authsql "github.com/0kaba0hub/yarilo/internal/auth/sql"
)

// buildCustomSchemaDB creates a SQLite DB with a custom-named table to
// exercise Dovecot-style password_query / user_query / iterate_query and
// AS-aliased columns.
func buildCustomSchemaDB(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "custom.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const ddl = `CREATE TABLE mailbox_users (
		email      TEXT PRIMARY KEY,
		pw_hash    TEXT NOT NULL,
		maildir    TEXT NOT NULL DEFAULT '',
		mail_path  TEXT NOT NULL DEFAULT '',
		active     INTEGER NOT NULL DEFAULT 1
	);`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO mailbox_users (email, pw_hash, maildir, mail_path, active) VALUES (?, ?, ?, ?, ?)`
	rows := []struct {
		email, pw, home, mail string
		active                int
	}{
		{"alice@example.com", "{PLAIN}wonderland", "/srv/alice", "maildir:/srv/alice/Maildir", 1},
		{"bob@example.com", "{PLAIN}bobpass", "/srv/bob", "maildir:/srv/bob/Maildir", 1},
		{"disabled@example.com", "{PLAIN}any", "", "", 0},
	}
	for _, r := range rows {
		if _, err := db.Exec(insert, r.email, r.pw, r.home, r.mail, r.active); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func TestCustomPasswordQuery(t *testing.T) {
	dsn := buildCustomSchemaDB(t)
	p, err := authsql.New(authsql.Config{
		Driver:     "sqlite",
		DSN:        dsn,
		SkipSchema: true,
		PasswordQuery: `SELECT pw_hash, maildir, mail_path, active
		                FROM mailbox_users WHERE email = %u`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	cases := []struct {
		name, user, pass string
		wantNil          bool
		wantResult       protocol.AuthResult
		wantHome         string
	}{
		{"alice ok", "alice@example.com", "wonderland", false, protocol.AuthOK, "/srv/alice"},
		{"alice wrong pass", "alice@example.com", "wrong", false, protocol.AuthFail, ""},
		{"disabled rejected", "disabled@example.com", "any", false, protocol.AuthFail, ""},
		{"unknown returns nil", "nobody@example.com", "x", true, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := p.Authenticate(tc.user, tc.pass, "imap")
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if tc.wantNil {
				if resp != nil {
					t.Fatalf("expected nil, got %+v", resp)
				}
				return
			}
			if resp == nil {
				t.Fatal("expected non-nil response")
			}
			if resp.Result != tc.wantResult {
				t.Errorf("result %v, want %v", resp.Result, tc.wantResult)
			}
			if tc.wantHome != "" && resp.Home != tc.wantHome {
				t.Errorf("home %q, want %q", resp.Home, tc.wantHome)
			}
		})
	}
}

func TestCustomUserQueryOverridesPasswdHomeAndMail(t *testing.T) {
	dsn := buildCustomSchemaDB(t)
	// Password query intentionally returns empty home/mail.
	p, err := authsql.New(authsql.Config{
		Driver:     "sqlite",
		DSN:        dsn,
		SkipSchema: true,
		PasswordQuery: `SELECT pw_hash, '' AS home, '' AS mail, active
		                FROM mailbox_users WHERE email = %u`,
		// Separate userdb query fills home/mail from the same table.
		UserQuery: `SELECT maildir, mail_path FROM mailbox_users WHERE email = %u`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	resp, err := p.Authenticate("alice@example.com", "wonderland", "imap")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		t.Fatalf("expected AuthOK, got %+v", resp)
	}
	if resp.Home != "/srv/alice" {
		t.Errorf("home: got %q, want /srv/alice", resp.Home)
	}
	if resp.MailLoc != "maildir:/srv/alice/Maildir" {
		t.Errorf("mail: got %q, want maildir:/srv/alice/Maildir", resp.MailLoc)
	}
}

func TestLookupUser_NotConfigured(t *testing.T) {
	p, _ := openTestDB(t)
	if _, _, err := p.LookupUser("anyone"); err == nil {
		t.Fatal("expected error when user_query unset")
	}
}

func TestIterate(t *testing.T) {
	dsn := buildCustomSchemaDB(t)
	p, err := authsql.New(authsql.Config{
		Driver:       "sqlite",
		DSN:          dsn,
		SkipSchema:   true,
		IterateQuery: `SELECT email FROM mailbox_users WHERE active = 1 ORDER BY email`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	got, err := p.Iterate()
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	want := []string{"alice@example.com", "bob@example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestIterate_NotConfigured(t *testing.T) {
	p, _ := openTestDB(t)
	if _, err := p.Iterate(); err == nil {
		t.Fatal("expected error when iterate_query unset")
	}
}

func TestSkipSchema_PreventsTableCreation(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "no-schema.db")
	p, err := authsql.New(authsql.Config{
		Driver:     "sqlite",
		DSN:        dsn,
		SkipSchema: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	db, _ := sql.Open("sqlite", dsn)
	defer db.Close()
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='yarilo_users'`).Scan(&name)
	if err == nil {
		t.Errorf("expected no yarilo_users table when skip_schema=true, found %q", name)
	}
}

func TestDefaultPassScheme_BcryptForBareHash(t *testing.T) {
	// Set up a DB where stored password has no {SCHEME} prefix and no
	// crypt(3) marker — but default_pass_scheme=BCRYPT so it's treated as one.
	dsn := filepath.Join(t.TempDir(), "default.db")
	db, _ := sql.Open("sqlite", dsn)
	if _, err := db.Exec(`CREATE TABLE users (email TEXT PRIMARY KEY, pw TEXT, active INTEGER DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	// Plain-prefixed because we want a sanity baseline. The default_scheme
	// only matters when no prefix is detected.
	if _, err := db.Exec(`INSERT INTO users VALUES ('carol@example.com', 'literalpass', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	p, err := authsql.New(authsql.Config{
		Driver:            "sqlite",
		DSN:               dsn,
		SkipSchema:        true,
		PasswordQuery:     `SELECT pw, '' AS home, '' AS mail, active FROM users WHERE email = %u`,
		DefaultPassScheme: "PLAIN",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	resp, _ := p.Authenticate("carol@example.com", "literalpass", "imap")
	if resp == nil || resp.Result != protocol.AuthOK {
		t.Fatalf("expected AuthOK with default_pass_scheme=PLAIN, got %+v", resp)
	}
}
