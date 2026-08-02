package authtoken_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yarilomail/yarilo/pkg/authtoken"
)

// store is the subset of both Store and RedisStore that tests exercise.
type store interface {
	Issue(username, sessionID, service string) (string, error)
	Validate(tok string) (username, sessionID, service string, ok bool)
	Close()
}

func runSuite(t *testing.T, s store) {
	t.Helper()

	t.Run("IssueValidate", func(t *testing.T) {
		tok, err := s.Issue("alice@example.com", "sess-1", "imap")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("token length: got %d, want 64", len(tok))
		}
		u, sid, svc, ok := s.Validate(tok)
		if !ok {
			t.Fatal("Validate: expected ok=true")
		}
		if u != "alice@example.com" {
			t.Errorf("username: got %q, want alice@example.com", u)
		}
		if sid != "sess-1" {
			t.Errorf("sessionID: got %q, want sess-1", sid)
		}
		if svc != "imap" {
			t.Errorf("service: got %q, want imap", svc)
		}
	})

	t.Run("OneTimeUse", func(t *testing.T) {
		tok, _ := s.Issue("bob@example.com", "sess-2", "imap")
		s.Validate(tok) // consume
		_, _, _, ok := s.Validate(tok)
		if ok {
			t.Fatal("second Validate should return ok=false (one-time use)")
		}
	})

	t.Run("UnknownToken", func(t *testing.T) {
		_, _, _, ok := s.Validate("0000000000000000000000000000000000000000000000000000000000000000")
		if ok {
			t.Fatal("unknown token should return ok=false")
		}
	})
}

func TestMemoryStore(t *testing.T) {
	s := authtoken.New(5 * time.Second)
	defer s.Close()
	runSuite(t, s)
}

func TestMemoryStore_Expired(t *testing.T) {
	s := authtoken.New(10 * time.Millisecond)
	defer s.Close()

	tok, _ := s.Issue("carol@example.com", "sess-3", "imap")
	time.Sleep(20 * time.Millisecond)

	_, _, _, ok := s.Validate(tok)
	if ok {
		t.Fatal("expired token should return ok=false")
	}
}

func TestRedisStore(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	s := authtoken.NewRedis(rdb, 5*time.Second)
	defer s.Close()
	runSuite(t, s)
}

// TestRedisStore_KeyPrefix pins that WithKeyPrefix actually namespaces the token
// keys (#939): the issued key lives under the custom prefix, not the default, so
// two installations on one Redis do not collide.
func TestRedisStore_KeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	s := authtoken.NewRedis(rdb, 5*time.Second, authtoken.WithKeyPrefix("tenant-a:tok:"))
	tok, err := s.Issue("alice@example.com", "sess-1", "imap")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !mr.Exists("tenant-a:tok:" + tok) {
		t.Fatalf("token key not under the configured prefix; keys = %v", mr.Keys())
	}
	if mr.Exists("yarilo:authtoken:" + tok) {
		t.Fatal("token key leaked under the default prefix")
	}
	// And the store still validates its own token round-trip.
	if _, _, _, ok := s.Validate(tok); !ok {
		t.Fatal("validate should succeed for a token issued under the custom prefix")
	}
}

// TestRedisStore_EmptyPrefixKeepsDefault: a blank WithKeyPrefix must not erase
// the namespace.
func TestRedisStore_EmptyPrefixKeepsDefault(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	s := authtoken.NewRedis(rdb, 5*time.Second, authtoken.WithKeyPrefix(""))
	tok, _ := s.Issue("bob@example.com", "sess-2", "imap")
	if !mr.Exists("yarilo:authtoken:" + tok) {
		t.Fatalf("empty prefix should keep the default; keys = %v", mr.Keys())
	}
}

func TestRedisStore_Expired(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	s := authtoken.NewRedis(rdb, time.Second)
	tok, _ := s.Issue("dave@example.com", "sess-4", "smtp")

	mr.FastForward(2 * time.Second)

	_, _, _, ok := s.Validate(tok)
	if ok {
		t.Fatal("expired token should return ok=false")
	}
}
