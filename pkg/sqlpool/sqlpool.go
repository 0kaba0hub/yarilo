// Package sqlpool applies connection-pool limits to a database/sql handle.
// Go's default MaxIdleConns of 2 causes connection churn under bursts.
package sqlpool

import (
	"database/sql"
	"time"
)

const (
	// DefaultConnMaxLifetime recycles connections so they do not pin to a failed-over server.
	DefaultConnMaxLifetime = 5 * time.Minute
	// DefaultConnMaxIdleTime returns capacity to the server when the process goes quiet.
	DefaultConnMaxIdleTime = time.Minute
)

// Per-driver open-connection defaults; connection cost differs widely between drivers.
const (
	// MySQL connections are threads; stock max_connections is 151.
	mysqlMaxOpenConns = 25

	// Postgres forks a process per connection; stock max_connections is 100.
	postgresMaxOpenConns = 8

	// SQLite writers serialise inside the library; extra connections only produce SQLITE_BUSY.
	sqliteMaxOpenConns = 1
)

// DefaultMaxOpenConns returns the open-connection limit for a driver name
// ("mysql" | "postgres" | "sqlite"). Unrecognised names get the MySQL value.
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

// Config carries pool limits. Zero values select the documented defaults.
type Config struct {
	// Driver selects the per-driver default for MaxOpenConns: "mysql" |
	// "postgres" | "sqlite". Ignored when MaxOpenConns is set explicitly.
	Driver string
	// MaxOpenConns caps total connections (in use plus idle). 0 = per-driver
	// default; negative = unlimited.
	MaxOpenConns int
	// MaxIdleConns caps retained idle connections. 0 mirrors MaxOpenConns.
	MaxIdleConns int
	// ConnMaxLifetimeSeconds recycles a connection after this age. 0 = default;
	// negative = never.
	ConnMaxLifetimeSeconds int
	// ConnMaxIdleTimeSeconds closes a connection idle for this long. 0 = default;
	// negative = never.
	ConnMaxIdleTimeSeconds int
}

// Apply configures db in place. nil db is a no-op.
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
		// match the open limit rather than Go's 2, otherwise the difference
		// is re-dialled on every burst
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

// duration maps a seconds knob to time.Duration: 0 = default, negative = no limit.
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
