package protocol

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// namedPassdb is a Passdb that also reports a driver name.
type namedPassdb struct {
	name   string
	result Result
}

func (p namedPassdb) Authenticate(*Request) (Result, error) { return p.result, nil }
func (p namedPassdb) DriverName() string                    { return p.name }

// anonPassdb deliberately omits DriverName — the interface must stay optional
// so external and test implementations keep compiling.
type anonPassdb struct{ result Result }

func (p anonPassdb) Authenticate(*Request) (Result, error) { return p.result, nil }

func TestDriverLabel(t *testing.T) {
	tests := []struct {
		name string
		db   Passdb
		want string
	}{
		{"named driver", namedPassdb{name: "mysql"}, "mysql"},
		{"empty name falls back", namedPassdb{name: ""}, "unknown"},
		{"no DriverName method", anonPassdb{}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := driverLabel(tc.db); got != tc.want {
				t.Fatalf("driverLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResultLabel(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		err    error
		want   string
	}{
		{"ok", ResultOK, nil, "ok"},
		{"fail", ResultFail, nil, "fail"},
		{"tempfail", ResultTempFail, nil, "tempfail"},
		{"next", ResultNext, nil, "next"},
		{"error wins over result", ResultOK, errStub{}, "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultLabel(tc.result, tc.err); got != tc.want {
				t.Fatalf("resultLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

type errStub struct{}

func (errStub) Error() string { return "stub" }

// TestCacheLookupMetricTaxonomy is the point of #881 for the cache: Cache
// itself collapses expired and pwd_mismatch into one miss counter, so only the
// metric split can tell an operator that the TTL is too short (expired) versus
// a stale credential being retried (pwd_mismatch).
func TestCacheLookupMetricTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(c *Cache)
		lookup  func(c *Cache) (*CacheEntry, bool)
		want    string
		wantHit bool
	}{
		{
			name:    "absent key is miss",
			prepare: func(*Cache) {},
			lookup:  func(c *Cache) (*CacheEntry, bool) { return c.Lookup("svc\tnobody", "pw") },
			want:    "miss",
		},
		{
			name: "valid entry is hit",
			prepare: func(c *Cache) {
				c.Insert("svc\tu1", "u1", "pw", ResultOK, NewFields())
			},
			lookup:  func(c *Cache) (*CacheEntry, bool) { return c.Lookup("svc\tu1", "pw") },
			want:    "hit",
			wantHit: true,
		},
		{
			name: "wrong password is pwd_mismatch",
			prepare: func(c *Cache) {
				c.Insert("svc\tu2", "u2", "pw", ResultOK, NewFields())
			},
			lookup: func(c *Cache) (*CacheEntry, bool) { return c.Lookup("svc\tu2", "wrong") },
			want:   "pwd_mismatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(1<<20, time.Minute, time.Minute)
			tc.prepare(c)
			before := testutil.ToFloat64(cacheLookups.WithLabelValues(tc.want))
			_, ok := tc.lookup(c)
			if ok != tc.wantHit {
				t.Fatalf("Lookup hit = %v, want %v", ok, tc.wantHit)
			}
			if got := testutil.ToFloat64(cacheLookups.WithLabelValues(tc.want)); got != before+1 {
				t.Fatalf("%s counter = %v, want %v", tc.want, got, before+1)
			}
		})
	}
}

func TestCacheLookupExpiredIsItsOwnLabel(t *testing.T) {
	c := NewCache(1<<20, time.Nanosecond, time.Nanosecond)
	c.Insert("svc\tu3", "u3", "pw", ResultOK, NewFields())
	time.Sleep(time.Millisecond)

	before := testutil.ToFloat64(cacheLookups.WithLabelValues("expired"))
	if _, ok := c.Lookup("svc\tu3", "pw"); ok {
		t.Fatal("expired entry must not hit")
	}
	if got := testutil.ToFloat64(cacheLookups.WithLabelValues("expired")); got != before+1 {
		t.Fatalf("expired counter = %v, want %v", got, before+1)
	}
}

func TestCacheSizeGaugesTrackOccupancy(t *testing.T) {
	c := NewCache(1<<20, time.Minute, time.Minute)
	c.Insert("svc\tu4", "u4", "pw", ResultOK, NewFields())

	if got := testutil.ToFloat64(cacheEntries); got < 1 {
		t.Fatalf("cache_entries = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(cacheBytes); got <= 0 {
		t.Fatalf("cache_bytes = %v, want > 0", got)
	}
	if got := testutil.ToFloat64(cacheMaxBytes); got != float64(1<<20) {
		t.Fatalf("cache_max_bytes = %v, want %v", got, 1<<20)
	}
}
