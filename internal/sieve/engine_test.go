package sieve

import (
	"context"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/memory"
)

func newTestDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newTestEngine(t *testing.T, d dict.Dict) *Engine {
	t.Helper()
	return New(config.SieveConfig{
		Enabled:       true,
		MaxRedirects:  32,
		MaxScriptSize: 65536,
	}, d)
}

const testMsg = "From: sender@example.com\r\nTo: user@example.com\r\nSubject: Test\r\n\r\nHello.\r\n"

func baseOpts(username string) FilterOptions {
	return FilterOptions{
		Username: username,
		HomeDir:  "/home/" + username,
		EnvFrom:  "sender@example.com",
		EnvTo:    username + "@example.com",
		MsgRaw:   []byte(testMsg),
	}
}

func TestFilterNoScript(t *testing.T) {
	d := newTestDict(t)
	e := newTestEngine(t, d)

	result, err := e.Filter(context.Background(), baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for no-script user, got %+v", result)
	}
}

func TestFilterKeep(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	if err := SaveScript(ctx, d, "u1", "/home/u1", "test.sieve", []byte(`keep;`)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, d, "u1", "/home/u1", "test.sieve"); err != nil {
		t.Fatal(err)
	}

	e := newTestEngine(t, d)
	result, err := e.Filter(ctx, baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].Folder != "INBOX" {
		t.Fatalf("expected INBOX delivery, got %+v", result.Deliveries)
	}
}

func TestFilterDiscard(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	if err := SaveScript(ctx, d, "u1", "/home/u1", "test.sieve", []byte(`discard;`)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, d, "u1", "/home/u1", "test.sieve"); err != nil {
		t.Fatal(err)
	}

	e := newTestEngine(t, d)
	result, err := e.Filter(ctx, baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Deliveries) != 0 || result.Reject != nil {
		t.Fatalf("expected discard (empty deliveries, no reject), got %+v", result)
	}
}

func TestFilterFileInto(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	src := `require "fileinto";` + "\n" + `fileinto "Spam";`
	if err := SaveScript(ctx, d, "u1", "/home/u1", "test.sieve", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, d, "u1", "/home/u1", "test.sieve"); err != nil {
		t.Fatal(err)
	}

	e := newTestEngine(t, d)
	result, err := e.Filter(ctx, baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].Folder != "Spam" {
		t.Fatalf("expected Spam delivery, got %+v", result.Deliveries)
	}
}

func TestFilterReject(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	src := `require "reject";` + "\n" + `reject "Unwanted mail.";`
	if err := SaveScript(ctx, d, "u1", "/home/u1", "test.sieve", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, d, "u1", "/home/u1", "test.sieve"); err != nil {
		t.Fatal(err)
	}

	e := newTestEngine(t, d)
	result, err := e.Filter(ctx, baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Reject == nil {
		t.Fatal("expected reject, got nil")
	}
	if result.Reject.Enhanced {
		t.Fatalf("expected plain reject, got enhanced")
	}
}

func TestFilterHeaderMatch(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	src := `require "fileinto";` + "\n" +
		`if header :contains "Subject" "Test" { fileinto "TestBox"; }`
	if err := SaveScript(ctx, d, "u1", "/home/u1", "test.sieve", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, d, "u1", "/home/u1", "test.sieve"); err != nil {
		t.Fatal(err)
	}

	e := newTestEngine(t, d)
	result, err := e.Filter(ctx, baseOpts("u1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].Folder != "TestBox" {
		t.Fatalf("expected TestBox delivery, got %+v", result.Deliveries)
	}
}

func TestInitUser(t *testing.T) {
	d := newTestDict(t)
	ctx := context.Background()

	if err := InitUser(ctx, d, "u1", "/home/u1"); err != nil {
		t.Fatalf("InitUser: %v", err)
	}

	name, err := ActiveScriptName(ctx, d, "u1", "/home/u1")
	if err != nil {
		t.Fatal(err)
	}
	if name != DefaultScriptName {
		t.Fatalf("expected active=%q, got %q", DefaultScriptName, name)
	}

	// Second call must be a no-op.
	if err := InitUser(ctx, d, "u1", "/home/u1"); err != nil {
		t.Fatalf("InitUser (second): %v", err)
	}
}
