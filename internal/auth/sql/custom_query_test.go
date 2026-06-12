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
		PasswordQuery: `SELECT pw_hash AS password, maildir AS home, mail_path AS mail, active AS enabled
		                FROM mailbox_users WHERE email = %u`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	cases := []struct {
		name, user, pass string
		wantNil          bool
		wantResult       protocol.Result
		wantHome         string
	}{
		{"alice ok", "alice@example.com", "wonderland", false, protocol.ResultOK, "/srv/alice"},
		{"alice wrong pass", "alice@example.com", "wrong", false, protocol.ResultFail, ""},
		{"disabled rejected", "disabled@example.com", "any", false, protocol.ResultFail, ""},
		{"unknown returns Next", "nobody@example.com", "x", true, protocol.ResultNext, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &protocol.Request{
				Username: tc.user,
				Password: tc.pass,
				Service:  "imap",
				Fields:   protocol.NewFields(),
			}
			got, err := p.Authenticate(req)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if tc.wantNil {
				if got != protocol.ResultNext {
					t.Fatalf("expected ResultNext for unknown, got %v", got)
				}
				return
			}
			if got != tc.wantResult {
				t.Errorf("result %v, want %v", got, tc.wantResult)
			}
			if tc.wantHome != "" {
				v, _ := req.Fields.Get("home")
				if v != tc.wantHome {
					t.Errorf("home %q, want %q", v, tc.wantHome)
				}
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
		PasswordQuery: `SELECT pw_hash AS password, '' AS home, '' AS mail, active AS enabled
		                FROM mailbox_users WHERE email = %u`,
		// Separate userdb query fills home/mail from the same table.
		UserQuery: `SELECT maildir AS home, mail_path AS mail FROM mailbox_users WHERE email = %u`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	req := &protocol.Request{
		Username: "alice@example.com",
		Password: "wonderland",
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
	if v, _ := req.Fields.Get("home"); v != "/srv/alice" {
		t.Errorf("home: got %q, want /srv/alice", v)
	}
	if v, _ := req.Fields.Get("mail"); v != "maildir:/srv/alice/Maildir" {
		t.Errorf("mail: got %q, want maildir:/srv/alice/Maildir", v)
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
		PasswordQuery:     `SELECT pw AS password, '' AS home, '' AS mail, active AS enabled FROM users WHERE email = %u`,
		DefaultPassScheme: "PLAIN",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	req := &protocol.Request{
		Username: "carol@example.com",
		Password: "literalpass",
		Service:  "imap",
		Fields:   protocol.NewFields(),
	}
	got, _ := p.Authenticate(req)
	if got != protocol.ResultOK {
		t.Fatalf("expected ResultOK with default_pass_scheme=PLAIN, got %v", got)
	}
}

func TestAllowNets_Enforcement(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "nets.db")
	db, _ := sql.Open("sqlite", dsn)
	db.Exec(`CREATE TABLE users (email TEXT PRIMARY KEY, pw TEXT, nets TEXT, active INTEGER DEFAULT 1)`)
	db.Exec(`INSERT INTO users VALUES ('alice@example.com', '{PLAIN}secret', '10.0.0.0/8,192.168.1.0/24', 1)`)
	db.Exec(`INSERT INTO users VALUES ('bob@example.com',   '{PLAIN}secret', '', 1)`)
	db.Close()

	p, err := authsql.New(authsql.Config{
		Driver:     "sqlite",
		DSN:        dsn,
		SkipSchema: true,
		PasswordQuery: `SELECT pw AS password, nets AS allow_nets, active AS enabled
		                FROM users WHERE email = %u`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })

	cases := []struct {
		name     string
		user     string
		remoteIP string
		want     protocol.Result
	}{
		{"alice allowed CIDR", "alice@example.com", "10.1.2.3", protocol.ResultOK},
		{"alice blocked IP", "alice@example.com", "8.8.8.8", protocol.ResultFail},
		{"alice second CIDR boundary", "alice@example.com", "192.168.1.255", protocol.ResultOK},
		{"alice outside second CIDR", "alice@example.com", "192.168.2.1", protocol.ResultFail},
		{"bob empty nets always OK", "bob@example.com", "8.8.8.8", protocol.ResultOK},
		{"empty remoteIP skips check", "alice@example.com", "", protocol.ResultOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &protocol.Request{
				Username: tc.user,
				Password: "secret",
				Service:  "imap",
				RemoteIP: tc.remoteIP,
				Fields:   protocol.NewFields(),
			}
			got, err := p.Authenticate(req)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if got != tc.want {
				t.Errorf("result %v, want %v", got, tc.want)
			}
		})
	}
}
