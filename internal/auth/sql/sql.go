// Package sql implements SQL passdb/userdb for yarilo-auth.
// Supports SQLite, MySQL, PostgreSQL via a unified interface, with
// Dovecot-style customizable queries (password_query / user_query /
// iterate_query) and %u/%n/%d parameter substitution.
package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"

	_ "github.com/go-sql-driver/mysql" // MySQL driver
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver (registered as "pgx")
	_ "modernc.org/sqlite"             // SQLite driver (no cgo)
)

// Per-driver schema. CREATE TABLE IF NOT EXISTS is run on every New() so
// fresh installs work without manual migration. Skipped when Config.SkipSchema
// is set (for connecting to existing schemas).
const (
	schemaSQLite = `CREATE TABLE IF NOT EXISTS yarilo_users (
    username    TEXT PRIMARY KEY,
    password    TEXT NOT NULL,
    home        TEXT NOT NULL DEFAULT '',
    mail        TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1
);`

	schemaMySQL = `CREATE TABLE IF NOT EXISTS yarilo_users (
    username    VARCHAR(255) PRIMARY KEY,
    password    VARCHAR(255) NOT NULL,
    home        VARCHAR(255) NOT NULL DEFAULT '',
    mail        VARCHAR(255) NOT NULL DEFAULT '',
    enabled     TINYINT(1) NOT NULL DEFAULT 1
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`

	schemaPostgres = `CREATE TABLE IF NOT EXISTS yarilo_users (
    username    TEXT PRIMARY KEY,
    password    TEXT NOT NULL,
    home        TEXT NOT NULL DEFAULT '',
    mail        TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1
);`
)

// Default queries used when Config doesn't override them. They target the
// built-in yarilo_users schema. Column order in the SELECT is the contract:
// password, home, mail, enabled.
const (
	defaultPasswordQuery = `SELECT password, home, mail, enabled FROM yarilo_users WHERE username = %u`
	defaultUserQuery     = `SELECT home, mail FROM yarilo_users WHERE username = %u AND enabled = 1`
	defaultIterateQuery  = `SELECT username FROM yarilo_users WHERE enabled = 1`
)

// Config is the open-time configuration for a SQL passdb.
type Config struct {
	Driver            string // sqlite | mysql | postgres
	DSN               string
	PasswordQuery     string // optional; defaults to built-in yarilo_users schema
	UserQuery         string // optional; if set, userdb lookup uses this query
	IterateQuery      string // optional; for admin tooling (list users)
	DefaultPassScheme string // assumed scheme when stored password has no {SCHEME} prefix (default PLAIN)
	SkipSchema        bool   // do not auto-create yarilo_users
}

// Passdb is an SQL-backed passdb (and optional userdb) entry.
type Passdb struct {
	db            *sql.DB
	driver        string
	passwordQuery string
	userQuery     string
	iterateQuery  string
	defaultScheme string
}

// New opens an SQL passdb.
func New(c Config) (*Passdb, error) {
	drv, ok := sqlDriverName(c.Driver)
	if !ok {
		return nil, fmt.Errorf("auth/sql: unsupported driver %q (want sqlite|mysql|postgres)", c.Driver)
	}
	db, err := sql.Open(drv, c.DSN)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: open %s: %w", c.Driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth/sql: ping %s: %w", c.Driver, err)
	}
	if !c.SkipSchema {
		if schema, ok := schemaFor(c.Driver); ok {
			if _, err := db.Exec(schema); err != nil {
				db.Close()
				return nil, fmt.Errorf("auth/sql: schema %s: %w", c.Driver, err)
			}
		}
	}
	pw := c.PasswordQuery
	if pw == "" {
		pw = defaultPasswordQuery
	}
	return &Passdb{
		db:            db,
		driver:        c.Driver,
		passwordQuery: pw,
		userQuery:     c.UserQuery,
		iterateQuery:  c.IterateQuery,
		defaultScheme: c.DefaultPassScheme,
	}, nil
}

// Authenticate verifies req.Username / req.Password against the SQL
// store and writes user fields directly into req.Fields when the
// lookup succeeds. Phase AUTH-2 PR 2 wire — drivers no longer
// allocate their own AuthResponse; the Chain owns the bag and the
// Result enum drives chain control flow.
//
// Outcomes:
//
//	ResultNext       — row not found in this database; chain falls through
//	ResultTempFail   — backend / query error (the error return carries
//	                    the underlying cause for the server-side log)
//	ResultFail       — user found but disabled OR password mismatch
//	ResultOK         — verified; req.Fields populated with user / home / mail
//
// The optional UserQuery runs after the password check to enrich
// home / mail with userdb-style data (matches the pre-refactor
// behaviour exactly).
func (p *Passdb) Authenticate(req *protocol.Request) (protocol.Result, error) {
	query, args := substituteVars(p.driver, p.passwordQuery, req.Username)

	var storedPass, home, mailLoc string
	var enabled int
	err := p.db.QueryRowContext(context.Background(), query, args...).
		Scan(&storedPass, &home, &mailLoc, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.ResultNext, nil
	}
	if err != nil {
		return protocol.ResultTempFail, err
	}
	if enabled == 0 {
		return protocol.ResultFail, nil
	}
	if !checkPasswordWithDefault(storedPass, req.Password, p.defaultScheme) {
		return protocol.ResultFail, nil
	}
	if p.userQuery != "" {
		if h, m, err := p.lookupUser(req.Username); err == nil {
			home, mailLoc = h, m
		}
	}
	req.Fields.Set("user", req.Username)
	if home != "" {
		req.Fields.Set("home", home)
	}
	if mailLoc != "" {
		req.Fields.Set("mail", mailLoc)
	}
	return protocol.ResultOK, nil
}

// LookupUser runs the optional user_query and returns the userdb fields.
// Returns sql.ErrNoRows when the user does not exist.
func (p *Passdb) LookupUser(username string) (home, mailLoc string, err error) {
	if p.userQuery == "" {
		return "", "", errors.New("auth/sql: user_query not configured")
	}
	return p.lookupUser(username)
}

// LookupSCRAMSha256 satisfies protocol.SCRAMSha256Lookup. The SQL
// passdb runs its configured password_query, recognises the
// `{SCRAM-SHA-256}` scheme prefix on the password column, and
// returns the parsed ScramCredentials so the SCRAM-SHA-256 SASL
// mechanism can drive challenge-response without ever seeing a
// plain password.
//
// Returns (nil, nil) for any of: user unknown / user disabled /
// stored password not a SCRAM-SHA-256 verifier. The session-side
// SCRAM server treats nil as "fabricate a fake verifier" so the
// exchange completes with a uniform auth-failed outcome and an
// attacker cannot enumerate users.
func (p *Passdb) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return p.lookupSCRAM(username, ParseSCRAMSha256Credentials)
}

// LookupSCRAMSha1 satisfies protocol.SCRAMSha1Lookup. SHA-1
// counterpart of LookupSCRAMSha256 — uses the same password_query
// path; only the verifier-blob scheme prefix differs.
func (p *Passdb) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return p.lookupSCRAM(username, ParseSCRAMSha1Credentials)
}

func (p *Passdb) lookupSCRAM(username string, parse func(string) (*sasl.ScramCredentials, bool)) (*sasl.ScramCredentials, error) {
	query, args := substituteVars(p.driver, p.passwordQuery, username)
	var storedPass, home, mailLoc string
	var enabled int
	err := p.db.QueryRowContext(context.Background(), query, args...).
		Scan(&storedPass, &home, &mailLoc, &enabled)
	_, _ = home, mailLoc
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if enabled == 0 {
		return nil, nil
	}
	creds, ok := parse(storedPass)
	if !ok {
		return nil, nil
	}
	return creds, nil
}

func (p *Passdb) lookupUser(username string) (home, mailLoc string, err error) {
	query, args := substituteVars(p.driver, p.userQuery, username)
	err = p.db.QueryRowContext(context.Background(), query, args...).Scan(&home, &mailLoc)
	return home, mailLoc, err
}

// Iterate runs the iterate_query and returns the list of usernames.
// Returns an error if iterate_query is not configured.
func (p *Passdb) Iterate() ([]string, error) {
	if p.iterateQuery == "" {
		return nil, errors.New("auth/sql: iterate_query not configured")
	}
	rows, err := p.db.QueryContext(context.Background(), p.iterateQuery)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: iterate: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("auth/sql: iterate scan: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Close releases the database connection.
func (p *Passdb) Close() error {
	return p.db.Close()
}

func sqlDriverName(driver string) (string, bool) {
	switch driver {
	case "sqlite":
		return "sqlite", true
	case "mysql":
		return "mysql", true
	case "postgres":
		return "pgx", true
	}
	return "", false
}

func schemaFor(driver string) (string, bool) {
	switch driver {
	case "sqlite":
		return schemaSQLite, true
	case "mysql":
		return schemaMySQL, true
	case "postgres":
		return schemaPostgres, true
	}
	return "", false
}
