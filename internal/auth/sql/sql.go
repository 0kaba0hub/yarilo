// Package sql implements SQL passdb/userdb for yarilo-auth.
// Supports SQLite, MySQL, PostgreSQL via a unified interface, with
// customizable queries (password_query / user_query / iterate_query)
// and %u/%n/%d parameter substitution.
package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/auth/scheme"
	"github.com/0kaba0hub/yarilo/pkg/sqlpool"

	_ "github.com/go-sql-driver/mysql" // MySQL driver
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver (registered as "pgx")
	_ "modernc.org/sqlite"             // SQLite driver (no cgo)
)

// Per-driver schema, run as CREATE TABLE IF NOT EXISTS on every New()
// so fresh installs need no manual migration. Skipped when
// Config.SkipSchema is set.
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

// Default queries for the built-in yarilo_users schema, used when Config
// doesn't override them. Results match by column name, not position, so
// operators may alias columns (pw_hash AS password). Required column:
// password. Optional: home, mail, enabled (absent = active, so a WHERE
// active=1 guard works equally).
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
	// Pool bounds the connection pool. The zero value still yields a
	// bounded, reusing pool.
	Pool sqlpool.Config
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
	pool := c.Pool
	pool.Driver = c.Driver
	sqlpool.Apply(db, pool)
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
// store, writing user fields into req.Fields on success. The Chain owns
// the bag; the Result drives chain control flow.
//
// Outcomes:
//
//	ResultNext       — row not found here; chain falls through
//	ResultTempFail   — backend / query error (cause in the error return)
//	ResultFail       — user found but disabled OR password mismatch
//	ResultOK         — verified; req.Fields populated with user / home / mail
//
// Columns match by name, not position. Only "password" is required;
// "home", "mail", "enabled" are optional (absent enabled = active). The
// optional UserQuery runs after the password check to enrich home/mail.
func (p *Passdb) Authenticate(req *protocol.Request) (protocol.Result, error) {
	query, args := substituteVars(p.driver, p.passwordQuery, req.Username)

	row, err := scanRowByName(p.db, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.ResultNext, nil
	}
	if err != nil {
		return protocol.ResultTempFail, err
	}

	storedPass := row["password"]
	if storedPass == "" {
		return protocol.ResultTempFail, fmt.Errorf("auth/sql: password_query returned no 'password' column for %q", req.Username)
	}

	if v, ok := row["enabled"]; ok && !protocol.IsTruthy(v) {
		return protocol.ResultFail, nil
	}

	if !scheme.VerifyWithDefault(storedPass, req.Password, p.defaultScheme) {
		return protocol.ResultFail, nil
	}

	if nets := protocol.SplitCSV(row["allow_nets"]); len(nets) > 0 {
		if !ipInAllowNets(req.RemoteIP, nets) {
			slog.Warn("auth/sql: allow_nets rejected login",
				"user", req.Username,
				"remote_ip", req.RemoteIP,
				"allow_nets", row["allow_nets"],
			)
			return protocol.ResultFail, nil
		}
	}

	home := row["home"]
	mailLoc := row["mail"]
	if p.userQuery != "" {
		if h, m, err := p.lookupUser(req.Username); err == nil {
			if h != "" {
				home = h
			}
			if m != "" {
				mailLoc = m
			}
		}
	}

	req.Fields.Set("user", req.Username)
	if home != "" {
		req.Fields.Set("home", home)
	}
	if mailLoc != "" {
		req.Fields.Set("mail", mailLoc)
	}
	// Forward extra passdb fields (allow_nets, proxy, nologin, …) so the
	// auth protocol layer enforces them without passdb-specific knowledge.
	skipCols := map[string]bool{"password": true, "enabled": true, "home": true, "mail": true}
	for k, v := range row {
		if !skipCols[k] && v != "" {
			req.Fields.Set(k, v)
		}
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

// LookupSCRAMSha256 satisfies protocol.SCRAMSha256Lookup. Runs
// password_query, recognises the `{SCRAM-SHA-256}` scheme prefix, and
// returns the parsed ScramCredentials so the SASL mechanism drives
// challenge-response without a plain password.
//
// Returns (nil, nil) when the user is unknown / disabled / not a
// SCRAM-SHA-256 verifier. The SCRAM server fabricates a fake verifier on
// nil so the exchange fails uniformly and users cannot be enumerated.
func (p *Passdb) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	return p.lookupSCRAM(username, scheme.ParseSCRAMSha256Credentials)
}

// LookupSCRAMSha1 satisfies protocol.SCRAMSha1Lookup. SHA-1 counterpart
// of LookupSCRAMSha256; only the verifier-blob scheme prefix differs.
func (p *Passdb) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	return p.lookupSCRAM(username, scheme.ParseSCRAMSha1Credentials)
}

func (p *Passdb) lookupSCRAM(username string, parse func(string) (*sasl.ScramCredentials, bool)) (*sasl.ScramCredentials, error) {
	query, args := substituteVars(p.driver, p.passwordQuery, username)
	row, err := scanRowByName(p.db, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v, ok := row["enabled"]; ok && !protocol.IsTruthy(v) {
		return nil, nil
	}
	storedPass := row["password"]
	if storedPass == "" {
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
	row, err := scanRowByName(p.db, query, args...)
	if err != nil {
		return "", "", err
	}
	return row["home"], row["mail"], nil
}

// ipInAllowNets reports whether remoteIP is covered by any entry in nets.
// Entries may be CIDR (10.0.0.0/8) or a bare IP treated as /32 or /128.
// Returns true when nets is empty or remoteIP is empty (check not possible).
func ipInAllowNets(remoteIP string, nets []string) bool {
	if remoteIP == "" {
		return true
	}
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return true
	}
	for _, entry := range nets {
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			if cidr.Contains(ip) {
				return true
			}
			continue
		}
		if bare := net.ParseIP(entry); bare != nil && bare.Equal(ip) {
			return true
		}
	}
	return false
}

// scanRowByName executes query+args and returns the first row as a
// column-name → string map (integer/bool columns normalised to strings
// via stringify). Returns sql.ErrNoRows when no rows.
func scanRowByName(db *sql.DB, query string, args ...any) (map[string]string, error) {
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(cols))
	for i, col := range cols {
		result[col] = stringify(vals[i])
	}
	return result, rows.Err()
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

// DriverName satisfies protocol.DriverName so passdb metrics carry the SQL
// dialect ("mysql" | "postgres" | "sqlite") rather than a generic label.
func (p *Passdb) DriverName() string { return p.driver }
