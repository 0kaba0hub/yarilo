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

// Authenticate checks username/password against the SQL store. Returns nil
// (not found) to continue the passdb chain. The password_query must SELECT
// columns in this order: password, home, mail, enabled. Use SQL `AS` aliases
// to map an existing schema.
func (p *Passdb) Authenticate(username, password, _ string) (*protocol.AuthResponse, error) {
	query, args := substituteVars(p.driver, p.passwordQuery, username)

	var storedPass, home, mailLoc string
	var enabled int
	err := p.db.QueryRowContext(context.Background(), query, args...).
		Scan(&storedPass, &home, &mailLoc, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // pass to next passdb
	}
	if err != nil {
		return &protocol.AuthResponse{Result: protocol.AuthTempFail}, err
	}
	if enabled == 0 {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	if !checkPasswordWithDefault(storedPass, password, p.defaultScheme) {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	resp := &protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: username,
		Home:     home,
		MailLoc:  mailLoc,
	}
	if p.userQuery != "" {
		if h, m, err := p.lookupUser(username); err == nil {
			resp.Home, resp.MailLoc = h, m
		}
	}
	// Phase AUTH-2 PR 1: also populate the Fields bag so the wire
	// emitter (handleAuth → buildAuthOK) can switch over to the
	// bag-based path. The typed Home / MailLoc fields stay
	// populated in parallel for byte-compat with anything that
	// reads them directly until PR 2's Passdb interface swap.
	resp.Fields = protocol.NewFields()
	resp.Fields.Set("user", username)
	if resp.Home != "" {
		resp.Fields.Set("home", resp.Home)
	}
	if resp.MailLoc != "" {
		resp.Fields.Set("mail", resp.MailLoc)
	}
	return resp, nil
}

// LookupUser runs the optional user_query and returns the userdb fields.
// Returns sql.ErrNoRows when the user does not exist.
func (p *Passdb) LookupUser(username string) (home, mailLoc string, err error) {
	if p.userQuery == "" {
		return "", "", errors.New("auth/sql: user_query not configured")
	}
	return p.lookupUser(username)
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
