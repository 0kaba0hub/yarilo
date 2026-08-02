package sieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

func newImapEngine(t *testing.T, scriptDir string) *Engine {
	t.Helper()
	return New(config.SieveConfig{
		Enabled:            true,
		MaxRedirects:       32,
		MaxActions:         32,
		MaxScriptSize:      65536,
		DefaultName:        FallbackDefaultName,
		ImapSieveEnabled:   true,
		ImapSieveScriptDir: scriptDir,
	}, nil, nil, nil)
}

func TestRunIMAPEvent(t *testing.T) {
	dir := t.TempDir()
	// Bound script refiles on the COPY cause, keyed on the imap.cause env item.
	script := `require ["imapsieve", "environment", "fileinto"];` + "\n" +
		`if environment :is "imap.cause" "COPY" { fileinto "Reported"; }` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "report.sieve"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newImapEngine(t, dir)
	ctx := context.Background()
	raw := []byte("Subject: hi\r\n\r\nbody\r\n")

	// COPY cause → the guard matches → fileinto "Reported".
	res, err := e.RunIMAPEvent(ctx, IMAPEventOptions{
		Username: "u@x.com", Cause: "COPY", Mailbox: "Spam",
		SrcMailbox: "INBOX", MsgRaw: raw, ScriptName: "report",
	})
	if err != nil {
		t.Fatalf("RunIMAPEvent COPY: %v", err)
	}
	if res == nil || len(res.Deliveries) != 1 || res.Deliveries[0].Folder != "Reported" {
		t.Fatalf("COPY: expected fileinto Reported, got %+v", res)
	}

	// APPEND cause → the guard does not match → no fileinto action.
	res, err = e.RunIMAPEvent(ctx, IMAPEventOptions{
		Username: "u@x.com", Cause: "APPEND", Mailbox: "Spam",
		MsgRaw: raw, ScriptName: "report",
	})
	if err != nil {
		t.Fatalf("RunIMAPEvent APPEND: %v", err)
	}
	for _, d := range res.Deliveries {
		if d.Folder == "Reported" {
			t.Fatalf("APPEND must not fileinto Reported: %+v", res)
		}
	}
}

func TestRunIMAPEventDisabled(t *testing.T) {
	// Engine with imapsieve disabled returns (nil, nil).
	e := newTestEngine(t)
	res, err := e.RunIMAPEvent(context.Background(), IMAPEventOptions{
		Username: "u@x.com", Cause: "APPEND", Mailbox: "INBOX", ScriptName: "x",
	})
	if err != nil || res != nil {
		t.Fatalf("disabled imapsieve: expected (nil,nil), got (%+v, %v)", res, err)
	}
}

func TestImapEnvItems(t *testing.T) {
	e := &imapEnv{
		base:         &yariloEnv{username: "u@x.com"},
		cause:        "FLAG",
		mailbox:      "Archive",
		email:        "u@x.com",
		changedFlags: `\Seen \Flagged`,
		fromMailbox:  "INBOX",
		toMailbox:    "Archive",
	}
	cases := map[string]string{
		"imap.cause":              "FLAG",
		"imap.mailbox":            "Archive",
		"imap.user":               "u@x.com",
		"imap.changedflags":       `\Seen \Flagged`,
		"vnd.yarilo.mailbox-from": "INBOX",
		"vnd.yarilo.mailbox-to":   "Archive",
	}
	for item, want := range cases {
		got, ok := e.GetEnvironment(item)
		if !ok || got != want {
			t.Errorf("%s = %q (ok=%v), want %q", item, got, ok, want)
		}
	}
	// Falls through to the base env for vnd.yarilo.* items.
	if got, ok := e.GetEnvironment("vnd.yarilo.username"); !ok || got != "u@x.com" {
		t.Errorf("base fallthrough failed: %q %v", got, ok)
	}
}
