package imap_test

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureLogs redirects the default logger for the duration of a test.
type captureLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// TestCorruptFolderIsLoggedOnTheCommandThatHitsIt is the surface #1344 was
// found on: the folder is already selected and healthy, and the damage is
// discovered by a later reload during FETCH. The client gets CORRUPTION; the
// operator must get a line naming the account, the folder and the session --
// this is the gentle degradation where the diagnosis matters most, and where
// half the seam used to write nothing at all.
func TestCorruptFolderIsLoggedOnTheCommandThatHitsIt(t *testing.T) {
	prev := slog.Default()
	cap := &captureLogs{}
	slog.SetDefault(slog.New(slog.NewTextHandler(cap, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	addr, home := sandboxLikeServerWithHome(t)
	c := dialRaw(t, addr)
	if !strings.Contains(c.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("login")
	}
	if !strings.Contains(c.cmd("CREATE Broken"), "OK") {
		t.Fatal("create")
	}
	if !strings.Contains(c.cmd("SELECT Broken"), "OK") {
		t.Fatal("select")
	}

	damageIndex(t, filepath.Join(home, ".Broken", "yarilo.index"))

	got := c.cmd("FETCH 1:* (FLAGS)")
	if !strings.Contains(got, "CORRUPTION") {
		t.Errorf("FETCH over a damaged index must answer CORRUPTION: %q", got)
	}

	logged := cap.String()
	if !strings.Contains(logged, "folder index is unreadable") {
		t.Fatalf("nothing was logged for a folder that refuses inside a working account:\n%s", logged)
	}
	for _, want := range []string{"folder=Broken", "user=alice"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log line does not carry %s:\n%s", want, logged)
		}
	}
}
