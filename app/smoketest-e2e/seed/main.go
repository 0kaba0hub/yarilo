// seed creates the yarilo_users table (if missing) and inserts a single user
// with a bcrypt-hashed password. Intended for local smoke-test setup.
//
// Usage:
//
//	go run ./app/smoketest-e2e/seed /tmp/users.db alice@example.com wonderland
package main

import (
	"database/sql"
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: seed <sqlite-dsn> <username> <password>")
		os.Exit(2)
	}
	dsn, user, pass := os.Args[1], os.Args[2], os.Args[3]

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		fatal(err)
	}
	stored := "{BCRYPT}" + string(hash)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS yarilo_users (
		username TEXT PRIMARY KEY,
		password TEXT NOT NULL,
		home     TEXT NOT NULL DEFAULT '',
		mail     TEXT NOT NULL DEFAULT '',
		enabled  INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO yarilo_users (username, password, enabled) VALUES (?, ?, 1)`,
		user, stored,
	); err != nil {
		fatal(err)
	}
	fmt.Printf("seeded %s in %s\n", user, dsn)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
