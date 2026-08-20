package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #1344: a folder whose index cannot be read used to answer NO [SERVERBUG]
// "Internal server error" -- our own fault code, for what is the state of the
// data on disk, with nothing naming the folder. The account keeps working, so
// without the name the operator has one refusing mailbox somewhere among many.
//
// Driven over the wire with a genuinely damaged index rather than a stubbed
// error: what makes this reachable is the on-disk format, and a stub would
// pass whatever the format did.
func TestCorruptFolderIndexIsNamedAndTheAccountKeepsWorking(t *testing.T) {
	addr, home := sandboxLikeServerWithHome(t)
	c := dialRaw(t, addr)
	if !strings.Contains(c.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("login")
	}
	if !strings.Contains(c.cmd("CREATE Broken"), "OK") {
		t.Fatal("create")
	}
	if !strings.Contains(c.cmd("SELECT Broken"), "OK") {
		t.Fatal("select the folder we are about to damage")
	}
	if !strings.Contains(c.cmd("SELECT INBOX"), "OK") {
		t.Fatal("select INBOX")
	}

	damageIndex(t, filepath.Join(home, ".Broken", "yarilo.index"))

	// A fresh session, so nothing is served from the open handle.
	c2 := dialRaw(t, addr)
	if !strings.Contains(c2.cmd("LOGIN alice pw"), "OK") {
		t.Fatal("login")
	}
	got := c2.cmd("SELECT Broken")
	if !strings.Contains(got, "NO") {
		t.Fatalf("a damaged folder must be refused: %q", got)
	}
	if strings.Contains(got, "SERVERBUG") {
		t.Errorf("damaged data on disk is not our bug: %q", got)
	}
	if !strings.Contains(got, "CORRUPTION") {
		t.Errorf("the refusal must carry RFC 5530's CORRUPTION so a client knows retrying is pointless: %q", got)
	}
	if !strings.Contains(got, "Broken") {
		t.Errorf("the refusal must name the folder -- the rest of the account works: %q", got)
	}

	// The degradation is per folder, which is the whole reason the name matters.
	if !strings.Contains(c2.cmd("SELECT INBOX"), "OK") {
		t.Error("INBOX must still be selectable while another folder is damaged")
	}
	if !strings.Contains(c2.cmd(`LIST "" "*"`), "INBOX") {
		t.Error("LIST must still work")
	}
}

// damageIndex writes an unreadable major version into a folder's index, the
// way the field found it, and moves the file's mtime forward.
//
// The mtime is not decoration: reload has a fast path that skips the read when
// the base mtime and log size are unchanged, and rewriting a byte in place
// changes neither. Whether the damage was noticed then depended on the
// filesystem's clock resolution -- the test passed locally and skipped the
// reload entirely in CI, which is the flake this comment exists to prevent.
func damageIndex(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	raw[0] = 67
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("bump index mtime: %v", err)
	}
}
