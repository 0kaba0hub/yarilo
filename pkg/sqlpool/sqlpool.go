// Package sqlpool applies connection-pool limits to a database/sql handle.
//
// It exists because every sql.Open in this repository left Go's defaults in
// place, and the important one is MaxIdleConns = 2: under a burst, every
// concurrent query beyond the second opens a fresh connection and throws it away
// afterwards. That is the same connection-churn problem #878 removed from the
// login path, one layer down — and it is measurable, not theoretical. On the
// sandbox MySQL reported (#886):
//
//	Connections            225871
//	Threads_created         41304     (thread_cache_size = 9)
//	Max_used_connections      152
//	max_connections           151     ← ceiling reached
//	Aborted_connects          478
//
// Max_used_connections exceeding max_connections means the server refused
// connections. When that happens a passdb lookup returns temp-fail, which the
// login path surfaces to the client as NO [UNAVAILABLE].
package sqlpool

import (
	"database/sql"
	"time"
)

// Defaults chosen for a mail workload, where SQL is used for short point lookups
// (passdb/userdb) and small writes (quota mirroring), never long analytics.
const (
	// DefaultConnMaxLifetime recycles connections so they do not pin to a
	// failed-over server or accumulate server-side state indefinitely.
	DefaultConnMaxLifetime = 5 * time.Minute
	// DefaultConnMaxIdleTime returns capacity to the server when a process goes
	// quiet, without giving up reuse during a burst.
	DefaultConnMaxIdleTime = time.Minute
)

// The open-connection default is per driver, because the cost of a connection is
// not remotely comparable between them and one value would be wrong twice over.
const (
	// mysqlMaxOpenConns: a connection is a thread, so they are cheap to hold.
	// The sandbox server's max_connections is the stock 151, which a handful of
	// components at this size stays comfortably under.
	mysqlMaxOpenConns = 25

	// postgresMaxOpenConns is deliberately lower. Postgres forks a process per
	// connection (several MB each) and its stock max_connections is 100 — LOWER
	// than the MySQL ceiling this package was written after hitting. With a dozen
	// components in a deployment, 25 apiece would exhaust a default Postgres
	// faster than it exhausted MySQL.
	postgresMaxOpenConns = 8

	// sqliteMaxOpenConns is 1 on purpose. SQLite is a file, not a server:
	// concurrent writers serialise inside the library, and extra connections buy
	// SQLITE_BUSY errors rather than throughput. One connection makes the
	// serialisation explicit instead of surfacing it as intermittent failures.
	sqliteMaxOpenConns = 1
)

// DefaultMaxOpenConns returns the open-connection limit for a driver name as
// spelled in configuration ("mysql" | "postgres" | "sqlite"). An unrecognised
// name gets the MySQL value, which is the conservative middle of the three.
func DefaultMaxOpenConns(driver string) int {
	switch driver {
	case "postgres":
		return postgresMaxOpenConns
	case "sqlite":
		return sqliteMaxOpenConns
	default:
		return mysqlMaxOpenConns
	}
}

// Config carries pool limits. Zero values select the documented defaults, so a
// caller that passes an empty Config still gets a bounded, reusing pool rather
// than Go's two-idle-connection default.
type Config struct {
	// Driver selects the per-driver default for MaxOpenConns: "mysql" |
	// "postgres" | "sqlite". Ignored when MaxOpenConns is set explicitly.
	Driver string
	// MaxOpenConns caps total connections (in use plus idle). 0 = per-driver
	// default (see DefaultMaxOpenConns);
	// negative = unlimited, which is what the code did before this package and is
	// almost never what an operator wants.
	MaxOpenConns int
	// MaxIdleConns caps retained idle connections. 0 mirrors MaxOpenConns, which
	// is the actual churn fix: a pool that opens 25 connections but retains 2
	// re-dials 23 of them on every burst.
	MaxIdleConns int
	// ConnMaxLifetimeSeconds recycles a connection after this age. 0 = default;
	// negative = never.
	ConnMaxLifetimeSeconds int
	// ConnMaxIdleTimeSeconds closes a connection idle for this long. 0 = default;
	// negative = never.
	ConnMaxIdleTimeSeconds int
}

// Apply configures db in place. Safe to call on a nil db (no-op) so callers do
// not need to branch.
func Apply(db *sql.DB, c Config) {
	if db == nil {
		return
	}

	maxOpen := c.MaxOpenConns
	switch {
	case maxOpen == 0:
		maxOpen = DefaultMaxOpenConns(c.Driver)
	case maxOpen < 0:
		maxOpen = 0 // database/sql: 0 means unlimited
	}
	db.SetMaxOpenConns(maxOpen)

	maxIdle := c.MaxIdleConns
	if maxIdle == 0 {
		// Match the open limit rather than Go's 2. Retaining fewer idle
		// connections than the pool opens means the difference is re-dialled on
		// every burst, which is the churn this package exists to stop.
		maxIdle = maxOpen
		if maxIdle == 0 {
			maxIdle = DefaultMaxOpenConns(c.Driver)
		}
	}
	if maxIdle < 0 {
		maxIdle = 0
	}
	db.SetMaxIdleConns(maxIdle)

	db.SetConnMaxLifetime(duration(c.ConnMaxLifetimeSeconds, DefaultConnMaxLifetime))
	db.SetConnMaxIdleTime(duration(c.ConnMaxIdleTimeSeconds, DefaultConnMaxIdleTime))
}

// duration maps a seconds-valued knob onto a time.Duration: 0 takes the default,
// negative disables the limit (database/sql reads 0 as "no limit").
func duration(seconds int, def time.Duration) time.Duration {
	switch {
	case seconds == 0:
		return def
	case seconds < 0:
		return 0
	default:
		return time.Duration(seconds) * time.Second
	}
}
