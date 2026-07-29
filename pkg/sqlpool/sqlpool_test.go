package sqlpool

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestDefaultMaxOpenConnsPerDriver pins the values, because one number would be
// wrong twice: Postgres forks a process per connection and its stock
// max_connections (100) is LOWER than the MySQL ceiling this package was written
// after hitting, and SQLite is a file whose writers serialise inside the library.
func TestDefaultMaxOpenConnsPerDriver(t *testing.T) {
	tests := []struct {
		driver string
		want   int
	}{
		{"mysql", 25},
		{"postgres", 8},
		{"sqlite", 1},
		{"", 25},          // unset falls back to the middle value
		{"cockroach", 25}, // unrecognised likewise
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			if got := DefaultMaxOpenConns(tc.driver); got != tc.want {
				t.Fatalf("DefaultMaxOpenConns(%q) = %d, want %d", tc.driver, got, tc.want)
			}
		})
	}
	if DefaultMaxOpenConns("postgres") >= DefaultMaxOpenConns("mysql") {
		t.Fatal("the postgres default must stay below the mysql one — its connections are processes")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/pool.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestApplyIdleMatchesOpen is the actual churn fix. Go retains two idle
// connections by default, so a pool that opens 25 re-dials 23 of them on every
// burst — which is what drove 225k connections and 41k thread creations on the
// sandbox MySQL (#886).
func TestApplyIdleMatchesOpen(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "mysql"})

	stats := db.Stats()
	if stats.MaxOpenConnections != 25 {
		t.Fatalf("MaxOpenConnections = %d, want 25", stats.MaxOpenConnections)
	}
	// database/sql exposes no MaxIdleConns getter, so assert the observable
	// consequence: with idle capacity matching the open limit, a connection
	// returned to the pool is retained rather than closed.
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if idle := db.Stats().Idle; idle == 0 {
		t.Fatal("no idle connection retained after use — idle capacity did not follow the open limit")
	}
}

func TestApplyExplicitValues(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "sqlite", MaxOpenConns: 7, MaxIdleConns: 3})

	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7 (explicit value must beat the driver default)", got)
	}
}

// TestApplyNegativeMeansUnlimited covers the opt-out: database/sql reads 0 as
// "no limit", so a negative knob has to be translated rather than passed through.
func TestApplyNegativeMeansUnlimited(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "mysql", MaxOpenConns: -1})

	if got := db.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("MaxOpenConnections = %d, want 0 (unlimited)", got)
	}
}

func TestApplyNilDBIsSafe(t *testing.T) {
	// Callers should not have to branch.
	Apply(nil, Config{})
}

func TestDurationKnob(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		def     time.Duration
		want    time.Duration
	}{
		{"zero takes the default", 0, 5 * time.Minute, 5 * time.Minute},
		{"explicit seconds", 30, 5 * time.Minute, 30 * time.Second},
		{"negative disables", -1, 5 * time.Minute, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := duration(tc.seconds, tc.def); got != tc.want {
				t.Fatalf("duration(%d) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

// TestSQLiteDefaultSerialises guards the reason SQLite gets 1: extra connections
// to a file database buy SQLITE_BUSY, not throughput.
func TestSQLiteDefaultSerialises(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "sqlite"})

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("sqlite MaxOpenConnections = %d, want 1", got)
	}
}
