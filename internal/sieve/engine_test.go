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
	}, nil, nil, nil)
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

func TestFilterMailboxID(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		wantFolder string
	}{
		{
			name:       "resolved id wins over fallback",
			script:     `require ["fileinto", "mailboxid"];` + "\n" + `fileinto :mailboxid "aabbcc" "Fallback";`,
			wantFolder: "Archive",
		},
		{
			name:       "unresolved id falls back to positional",
			script:     `require ["fileinto", "mailboxid"];` + "\n" + `fileinto :mailboxid "deadbeef" "Fallback";`,
			wantFolder: "Fallback",
		},
		{
			name:       "mailboxidexists true takes the branch",
			script:     `require ["fileinto", "mailboxid"];` + "\n" + `if mailboxidexists "aabbcc" { fileinto "Archive"; } else { fileinto "Fallback"; }`,
			wantFolder: "Archive",
		},
		{
			name:       "mailboxidexists false takes else",
			script:     `require ["fileinto", "mailboxid"];` + "\n" + `if mailboxidexists "deadbeef" { fileinto "Archive"; } else { fileinto "Fallback"; }`,
			wantFolder: "Fallback",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			store := newTestStore()
			homeDir := t.TempDir()
			ctx := context.Background()

			if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(tc.script)); err != nil {
				t.Fatal(err)
			}
			if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
				t.Fatal(err)
			}

			opts := baseOpts("u1", homeDir)
			opts.MailboxByID = func(_ context.Context, id string) (string, bool) {
				if id == "aabbcc" {
					return "Archive", true
				}
				return "", false
			}

			result, err := e.Filter(ctx, opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if len(result.Deliveries) != 1 || result.Deliveries[0].Folder != tc.wantFolder {
				t.Fatalf("expected %q delivery, got %+v", tc.wantFolder, result.Deliveries)
			}
		})
	}
}

func TestFilterMetadata(t *testing.T) {
	// annotation store: "/private/vip" on Archive = "yes"; server "/shared/policy" = "strict".
	mailboxMeta := func(_ context.Context, mbox, annotation string) (string, bool, error) {
		if mbox == "Archive" && annotation == "/private/vip" {
			return "yes", true, nil
		}
		return "", false, nil
	}
	serverMeta := func(_ context.Context, annotation string) (string, bool, error) {
		if annotation == "/shared/policy" {
			return "strict", true, nil
		}
		return "", false, nil
	}

	tests := []struct {
		name       string
		script     string
		wantFolder string
	}{
		{
			name:       "metadata value match",
			script:     `require ["mboxmetadata","fileinto"];` + "\n" + `if metadata "Archive" "/private/vip" "yes" { fileinto "Hit"; } else { fileinto "Miss"; }`,
			wantFolder: "Hit",
		},
		{
			name:       "metadataexists true",
			script:     `require ["mboxmetadata","fileinto"];` + "\n" + `if metadataexists "Archive" "/private/vip" { fileinto "Hit"; } else { fileinto "Miss"; }`,
			wantFolder: "Hit",
		},
		{
			name:       "metadataexists false",
			script:     `require ["mboxmetadata","fileinto"];` + "\n" + `if metadataexists "Archive" "/private/absent" { fileinto "Hit"; } else { fileinto "Miss"; }`,
			wantFolder: "Miss",
		},
		{
			name:       "servermetadata value match",
			script:     `require ["servermetadata","fileinto"];` + "\n" + `if servermetadata "/shared/policy" "strict" { fileinto "Hit"; } else { fileinto "Miss"; }`,
			wantFolder: "Hit",
		},
		{
			name:       "servermetadataexists false",
			script:     `require ["servermetadata","fileinto"];` + "\n" + `if servermetadataexists "/shared/none" { fileinto "Hit"; } else { fileinto "Miss"; }`,
			wantFolder: "Miss",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			store := newTestStore()
			homeDir := t.TempDir()
			ctx := context.Background()

			if err := store.SaveScript(ctx, "u1", homeDir, "test", []byte(tc.script)); err != nil {
				t.Fatal(err)
			}
			if err := store.SetActive(ctx, "u1", homeDir, "test"); err != nil {
				t.Fatal(err)
			}

			opts := baseOpts("u1", homeDir)
			opts.MailboxMetadata = mailboxMeta
			opts.ServerMetadata = serverMeta

			result, err := e.Filter(ctx, opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if len(result.Deliveries) != 1 || result.Deliveries[0].Folder != tc.wantFolder {
				t.Fatalf("expected %q delivery, got %+v", tc.wantFolder, result.Deliveries)
			}
		})
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
