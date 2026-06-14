package mdbox

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// altTestBackend creates a Backend with alt storage pointing at a
// temp subdirectory and opens a userMailbox for the given home.
func altTestBackend(t *testing.T) (*Backend, *userMailbox, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	alt := filepath.Join(base, "alt")

	b := New(WithAltStorage(alt))
	u := b.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return b, u, alt
}

func testSaveMsg(t *testing.T, u *userMailbox, body string) string {
	t.Helper()
	filename, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return filename
}

func testFetchBody(t *testing.T, u *userMailbox, filename string) string {
	t.Helper()
	rc, err := u.Fetch("INBOX", filename)
	if err != nil {
		t.Fatalf("Fetch %q: %v", filename, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// TestAltMove_BasicFlow saves two messages, moves one to alt via
// AltMove(Before=now), verifies the primary file is gone and the
// message is still fetchable from alt.
func TestAltMove_BasicFlow(t *testing.T) {
	_, u, altRoot := altTestBackend(t)

	oldBody := "From: old@example.com\r\nSubject: old\r\n\r\nold message\r\n"
	newBody := "From: new@example.com\r\nSubject: new\r\n\r\nnew message\r\n"

	oldFile := testSaveMsg(t, u, oldBody)
	newFile := testSaveMsg(t, u, newBody)

	// Both messages exist in primary.
	if testFetchBody(t, u, oldFile) == "" {
		t.Fatal("old message not fetchable before altmove")
	}

	// Move everything with Before=now+1s (matches both).
	stats, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(time.Second)})
	if err != nil {
		t.Fatalf("AltMove: %v", err)
	}
	if stats.Candidates == 0 {
		t.Errorf("expected candidates > 0, got 0")
	}
	if stats.Moved == 0 {
		t.Errorf("expected moved > 0, got 0")
	}

	// Alt directory must exist and contain m.<N> files.
	altStorage := filepath.Join(altRoot, storageDir)
	entries, err := os.ReadDir(altStorage)
	if err != nil {
		t.Fatalf("alt storage dir missing: %v", err)
	}
	hasMFile := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "m.") {
			hasMFile = true
			break
		}
	}
	if !hasMFile {
		t.Errorf("no m.<N> files in alt storage after altmove")
	}

	// Primary storage must have no m.<N> files (all moved).
	primEntries, _ := os.ReadDir(u.storagePath())
	for _, e := range primEntries {
		if strings.HasPrefix(e.Name(), "m.") {
			t.Errorf("primary still has m.<N> file after full altmove: %s", e.Name())
		}
	}

	// Both messages must still be fetchable (via alt fallback).
	if got := testFetchBody(t, u, oldFile); !bytes.Contains([]byte(got), []byte("old message")) {
		t.Errorf("old message not fetchable after altmove: %q", got)
	}
	if got := testFetchBody(t, u, newFile); !bytes.Contains([]byte(got), []byte("new message")) {
		t.Errorf("new message not fetchable after altmove: %q", got)
	}
}

// TestAltMove_BeforeFilter verifies that --before skips newer messages.
func TestAltMove_BeforeFilter(t *testing.T) {
	_, u, _ := altTestBackend(t)

	body := "From: x@x.com\r\n\r\nbody\r\n"
	filename := testSaveMsg(t, u, body)

	// Before = epoch (past) → nothing matches.
	stats, err := u.AltMove(AltMoveQuery{Before: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("AltMove(past): %v", err)
	}
	if stats.Candidates != 0 {
		t.Errorf("expected 0 candidates for past cutoff, got %d", stats.Candidates)
	}

	// Message must still be in primary.
	if got := testFetchBody(t, u, filename); got == "" {
		t.Error("message lost after no-op altmove")
	}
}

// TestAltMove_Reverse moves to alt then back to primary and verifies
// full round-trip fetch fidelity.
func TestAltMove_Reverse(t *testing.T) {
	_, u, _ := altTestBackend(t)

	body := "From: rt@example.com\r\n\r\nround-trip\r\n"
	filename := testSaveMsg(t, u, body)

	// Primary → alt.
	if _, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(time.Second)}); err != nil {
		t.Fatalf("primary→alt: %v", err)
	}

	// Alt → primary (Reverse=true).
	stats, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(time.Second), Reverse: true})
	if err != nil {
		t.Fatalf("alt→primary: %v", err)
	}
	if stats.Moved == 0 {
		t.Errorf("expected moved > 0 on reverse, got 0")
	}

	// Alt should be empty after reverse.
	altEntries, _ := os.ReadDir(u.altStoragePath())
	for _, e := range altEntries {
		if strings.HasPrefix(e.Name(), "m.") {
			t.Errorf("alt still has m.<N> after reverse: %s", e.Name())
		}
	}

	// Message fetchable from primary.
	if got := testFetchBody(t, u, filename); !bytes.Contains([]byte(got), []byte("round-trip")) {
		t.Errorf("message lost after reverse altmove: %q", got)
	}
}

// TestAltMove_DisabledReturnsError confirms that AltMove errors when
// alt storage is not configured.
func TestAltMove_DisabledReturnsError(t *testing.T) {
	b := New() // no WithAltStorage
	home := t.TempDir()
	u := b.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	_, err := u.AltMove(AltMoveQuery{})
	if err == nil {
		t.Error("expected error when alt storage not configured")
	}
}

// TestAltMove_PerUserAltDir verifies that UserInfo.AltDir (per-user
// path from userdb/auth) is honoured even when the Backend has no
// WithAltStorage template set. This is the storage.alt_dir config path.
func TestAltMove_PerUserAltDir(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	altDir := filepath.Join(base, "cold") // simulates UserInfo.AltDir already expanded

	b := New() // no WithAltStorage — template empty
	u := b.OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com",
		Home:     home,
		AltDir:   altDir,
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	if !u.AltEnabled() {
		t.Fatal("AltEnabled() = false, want true when UserInfo.AltDir is set")
	}

	body := "From: x@example.com\r\nSubject: cold\r\n\r\nbody\r\n"
	file := testSaveMsg(t, u, body)

	stats, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(time.Second)})
	if err != nil {
		t.Fatalf("AltMove: %v", err)
	}
	if stats.Moved != 1 {
		t.Errorf("Moved = %d, want 1", stats.Moved)
	}

	// Message still fetchable from alt tier.
	got := testFetchBody(t, u, file)
	if !bytes.Contains([]byte(got), []byte("body")) {
		t.Errorf("body after altmove = %q, want original body", got)
	}
}

// TestExpandAltPath verifies template expansion for the common cases.
func TestExpandAltPath(t *testing.T) {
	cases := []struct {
		tmpl     string
		username string
		want     string
	}{
		{"/cold/%d/%n", "alice@example.com", "/cold/example.com/alice"},
		{"/cold/%u", "alice@example.com", "/cold/alice@example.com"},
		{"/cold/%Lu", "Alice@Example.Com", "/cold/alice@example.com"},
		{"/cold/%Ld/%Ln", "Alice@Example.Com", "/cold/example.com/alice"},
		{"", "alice@example.com", ""},
	}
	for _, tc := range cases {
		got := expandAltPath(tc.tmpl, tc.username)
		if got != tc.want {
			t.Errorf("expandAltPath(%q, %q) = %q, want %q", tc.tmpl, tc.username, got, tc.want)
		}
	}
}
