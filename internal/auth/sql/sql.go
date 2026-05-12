// Package sql implements SQL passdb for yarilo-auth.
// Supports SQLite, MySQL, PostgreSQL via a unified interface.
package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"

	_ "modernc.org/sqlite" // SQLite driver (no cgo)
)

const schema = `
CREATE TABLE IF NOT EXISTS yarilo_users (
    username    TEXT PRIMARY KEY,
    password    TEXT NOT NULL,
    home        TEXT NOT NULL DEFAULT '',
    mail        TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1
);
`

// Passdb is an SQL-backed passdb entry.
type Passdb struct {
	db     *sql.DB
	driver string
}

// New opens an SQL passdb. driver: "sqlite", "mysql", or "postgres".
func New(driver, dsn string) (*Passdb, error) {
	drv := sqlDriver(driver)
	db, err := sql.Open(drv, dsn)
	if err != nil {
		return nil, fmt.Errorf("auth/sql: open %s: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth/sql: ping %s: %w", driver, err)
	}
	if driver == "sqlite" {
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("auth/sql: schema: %w", err)
		}
	}
	return &Passdb{db: db, driver: driver}, nil
}

// Authenticate checks username/password against the SQL store.
// Returns nil (not found) to continue the passdb chain.
func (p *Passdb) Authenticate(username, password, service string) (*protocol.AuthResponse, error) {
	var storedPass, home, mailLoc string
	var enabled int

	query := `SELECT password, home, mail, enabled FROM yarilo_users WHERE username = $1`
	if p.driver == "sqlite" || p.driver == "mysql" {
		query = strings.Replace(query, "$1", "?", 1)
	}

	err := p.db.QueryRowContext(context.Background(), query, username).
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
	if !checkPassword(storedPass, password) {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return &protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: username,
		Home:     home,
		MailLoc:  mailLoc,
	}, nil
}

// Close releases the database connection.
func (p *Passdb) Close() error {
	return p.db.Close()
}

func sqlDriver(driver string) string {
	switch driver {
	case "sqlite":
		return "sqlite"
	case "mysql":
		return "mysql"
	case "postgres":
		return "pgx"
	}
	return driver
}

// checkPassword compares a plain-text or {PLAIN} prefixed stored password.
// Production use should store bcrypt/argon2 hashes; this handles plain for dev.
func checkPassword(stored, input string) bool {
	if strings.HasPrefix(stored, "{PLAIN}") {
		return stored[7:] == input
	}
	return stored == input
}
