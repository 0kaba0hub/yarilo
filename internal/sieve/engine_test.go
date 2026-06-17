package sieve

import (
	"context"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return New(config.SieveConfig{
		Enabled:       true,
		MaxRedirects:  32,
		MaxScriptSize: 65536,
	}, nil, nil)
}

const testMsg = "From: sender@example.com\r\nTo: user@example.com\r\nSubject: Test\r\n\r\nHello.\r\n"

func baseOpts(username, homeDir string) FilterOptions {
	return FilterOptions{
		Username: username,
		HomeDir:  homeDir,
		EnvFrom:  "sender@example.com",
		EnvTo:    username + "@example.com",
		MsgRaw:   []byte(testMsg),
	}
}

func TestFilterNoScript(t *testing.T) {
	e := newTestEngine(t)
	homeDir := t.TempDir()

	result, err := e.Filter(context.Background(), baseOpts("u1", homeDir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for no-script user, got %+v", result)
	}
}

func TestFilterKeep(t *testing.T) {
	e := newTestEngine(t)
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := FsSaveScript(ctx, nil, homeDir, "test", []byte(`keep;`)); err != nil {
		t.Fatal(err)
	}
	if err := FsSetActive(ctx, nil, homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
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
	e := newTestEngine(t)
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := FsSaveScript(ctx, nil, homeDir, "test", []byte(`discard;`)); err != nil {
		t.Fatal(err)
	}
	if err := FsSetActive(ctx, nil, homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
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
	e := newTestEngine(t)
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "fileinto";` + "\n" + `fileinto "Spam";`
	if err := FsSaveScript(ctx, nil, homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := FsSetActive(ctx, nil, homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
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
	e := newTestEngine(t)
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "reject";` + "\n" + `reject "Unwanted mail.";`
	if err := FsSaveScript(ctx, nil, homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := FsSetActive(ctx, nil, homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
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
	e := newTestEngine(t)
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "fileinto";` + "\n" +
		`if header :contains "Subject" "Test" { fileinto "TestBox"; }`
	if err := FsSaveScript(ctx, nil, homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := FsSetActive(ctx, nil, homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	result, err := e.Filter(ctx, baseOpts("u1", homeDir))
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
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := FsInitUser(ctx, nil, homeDir); err != nil {
		t.Fatalf("FsInitUser: %v", err)
	}

	name, err := FsActiveScriptName(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("expected active=\"\" (default, no named script), got %q", name)
	}

	// Verify that yarilo.sieve exists and contains the default body.
	src, _, err := FsLoadActiveScript(homeDir)
	if err != nil {
		t.Fatalf("FsLoadActiveScript: %v", err)
	}
	if string(src) != DefaultScriptBody {
		t.Fatalf("expected %q, got %q", DefaultScriptBody, src)
	}

	// Second call must be a no-op.
	if err := FsInitUser(ctx, nil, homeDir); err != nil {
		t.Fatalf("FsInitUser (second): %v", err)
	}
}
