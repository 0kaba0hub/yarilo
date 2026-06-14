package authtoken_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/pkg/authtoken"
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
