package authtoken_test

import (
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/authtoken"
)

func TestIssueValidate(t *testing.T) {
	s := authtoken.New(5 * time.Second)
	defer s.Close()

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
		t.Errorf("username: got %q, want %q", u, "alice@example.com")
	}
	if sid != "sess-1" {
		t.Errorf("sessionID: got %q, want %q", sid, "sess-1")
	}
	if svc != "imap" {
		t.Errorf("service: got %q, want %q", svc, "imap")
	}
}

func TestOneTimeUse(t *testing.T) {
	s := authtoken.New(5 * time.Second)
	defer s.Close()

	tok, _ := s.Issue("bob@example.com", "sess-2", "imap")
	s.Validate(tok) // consume

	_, _, _, ok := s.Validate(tok)
	if ok {
		t.Fatal("second Validate should return ok=false (one-time use)")
	}
}

func TestExpired(t *testing.T) {
	s := authtoken.New(10 * time.Millisecond)
	defer s.Close()

	tok, _ := s.Issue("carol@example.com", "sess-3", "imap")
	time.Sleep(20 * time.Millisecond)

	_, _, _, ok := s.Validate(tok)
	if ok {
		t.Fatal("expired token should return ok=false")
	}
}

func TestUnknownToken(t *testing.T) {
	s := authtoken.New(5 * time.Second)
	defer s.Close()

	_, _, _, ok := s.Validate("0000000000000000000000000000000000000000000000000000000000000000")
	if ok {
		t.Fatal("unknown token should return ok=false")
	}
}
