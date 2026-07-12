package sieve

import (
	"context"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return New(config.SieveConfig{
		Enabled:       true,
		MaxRedirects:  32,
		MaxScriptSize: 65536,
		DefaultName:   FallbackDefaultName,
	}, nil, nil)
}

func newTestStore() *FsScriptStore {
	return &FsScriptStore{DefaultName: FallbackDefaultName, Locker: nil}
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
	result, err := e.Filter(context.Background(), baseOpts("u1", t.TempDir()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for no-script user, got %+v", result)
	}
}

func TestFilterKeep(t *testing.T) {
	e := newTestEngine(t)
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(`keep;`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
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
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(`discard;`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
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
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "fileinto";` + "\n" + `fileinto "Spam";`
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
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
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "reject";` + "\n" + `reject "Unwanted mail.";`
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
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
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	src := `require "fileinto";` + "\n" +
		`if header :contains "Subject" "Test" { fileinto "TestBox"; }`
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(src)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
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
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	if err := store.InitUser(ctx, "u1", homeDir); err != nil {
		t.Fatalf("InitUser: %v", err)
	}

	name, err := store.ActiveScriptName(context.Background(), "u1", homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("expected active=\"\" (default, no named script), got %q", name)
	}

	src, _, err := store.LoadActiveScript(context.Background(), "u1", homeDir)
	if err != nil {
		t.Fatalf("LoadActiveScript: %v", err)
	}
	if string(src) != DefaultScriptBody {
		t.Fatalf("expected %q, got %q", DefaultScriptBody, src)
	}

	if err := store.InitUser(ctx, "u1", homeDir); err != nil {
		t.Fatalf("InitUser (second): %v", err)
	}
}

func TestFilterMIME_ForEveryPartMimeExtractText(t *testing.T) {
	e := newTestEngine(t)
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	script := `require ["foreverypart","mime","extracttext","variables","fileinto"];
foreverypart {
    if header :mime :subtype "Content-Type" "html" {
        fileinto "HTML";
    }
}`
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts("u1", homeDir)
	opts.MsgRaw = []byte("Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n\r\ntext\r\n" +
		"--b\r\n" +
		"Content-Type: text/html\r\n\r\n<p>h</p>\r\n" +
		"--b--\r\n")

	result, err := e.Filter(ctx, opts)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if result == nil || len(result.Deliveries) != 1 || result.Deliveries[0].Folder != "HTML" {
		t.Fatalf("expected fileinto HTML from foreverypart+mime, got %+v", result)
	}
}

func TestFilterMIME_ReplaceRedactsPart(t *testing.T) {
	e := newTestEngine(t)
	store := newTestStore()
	homeDir := t.TempDir()
	ctx := context.Background()

	script := `require ["foreverypart","mime","replace"];
foreverypart {
    if header :mime :subtype "Content-Type" "html" {
        replace "REDACTED";
    }
}`
	if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts("u1", homeDir)
	opts.MsgRaw = []byte("Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<p>secret</p>\r\n--b--\r\n")

	result, err := e.Filter(ctx, opts)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if result.Message == nil {
		t.Fatal("replace should substitute the delivered message (result.Message)")
	}
	out := string(result.Message)
	if strings.Contains(out, "secret") {
		t.Errorf("replace should have redacted the html part; got:\n%s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED in filtered message:\n%s", out)
	}
}
