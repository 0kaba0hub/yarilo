package mailboxbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The on-disk folder name is asserted by reading the directory, not by reading
// back through the same driver that wrote it: a driver that encodes and decodes
// with the same wrong rule is self-consistent and tells us nothing.
func diskFolderName(t *testing.T, home string) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(home, "Maildir"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") && n != "." && n != ".." {
			return n
		}
	}
	t.Fatal("no folder directory found")
	return ""
}

func loadWith(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func createFolder(t *testing.T, sc config.StorageConfig, name string) string {
	t.Helper()
	home := t.TempDir()
	u := ByDriver("maildir", sc, nil).OpenUser(&mailbox.UserInfo{
		Username: "u@d.test", Home: home, Driver: "maildir", Separator: "/",
	})
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	// Normalisation moved out of the driver to the name-entry boundary
	// (mailbox.NormalizeName, driven by mailbox_list_normalize_to_nfc). A driver
	// call bypasses that boundary, so the test applies it here the way a session
	// resolver does -- otherwise it would assert the driver still owns the
	// transform, which is exactly what #1113 removed.
	name = mailbox.NormalizeName(name, !sc.MailboxListNormalizeToNFC)
	if err := u.Create(name); err != nil {
		t.Fatal(err)
	}
	return diskFolderName(t, home)
}

// values.yaml promises UTF-8 on disk by default. Neither key was defaulted, so
// ByDriver passed false and names were written as modified-UTF-7 instead --
// documented one way, behaving the other, with no way to correct it because the
// key was never rendered (#1074).
func TestDefaultConfigWritesFolderNamesAsUTF8(t *testing.T) {
	cfg := loadWith(t, "storage:\n  mailbox: maildir\n")
	got := createFolder(t, cfg.Storage, "Вхідні")
	if want := ".Вхідні"; got != want {
		t.Errorf("on disk %q, want %q — the documented default is UTF-8", got, want)
	}
}

// The legacy setting has to work, because it is the reason the key exists: an
// installation already storing modified-UTF-7 names sets it to keep reading its
// own mail.
func TestMailboxListUTF8FalseWritesModifiedUTF7(t *testing.T) {
	cfg := loadWith(t, "storage:\n  mailbox: maildir\n  mailbox_list_utf8: false\n")
	got := createFolder(t, cfg.Storage, "Вхідні")
	if !strings.HasPrefix(got, ".&") {
		t.Errorf("on disk %q, want a modified-UTF-7 name beginning %q", got, ".&")
	}
	if got == ".Вхідні" {
		t.Error("mailbox_list_utf8: false had no effect — the setting is decorative")
	}
}

// NFC normalisation is likewise documented as on by default. Asserted through
// the bytes: the decomposed and composed spellings must land on one directory.
func TestDefaultConfigNormalisesToNFC(t *testing.T) {
	cfg := loadWith(t, "storage:\n  mailbox: maildir\n")
	// "e" + U+0301, five runes, not the composed four-rune spelling. The
	// difference is invisible in an editor, so it is worth stating: a
	// composed literal would make both NFC tests pass whatever the setting does.
	decomposed := "Café"
	got := createFolder(t, cfg.Storage, decomposed)
	if want := ".Caf\u00e9"; got != want {
		t.Errorf("on disk %q, want %q — decomposed input should be stored NFC", got, want)
	}
}

func TestMailboxListNormalizeToNFCFalseKeepsTheInputForm(t *testing.T) {
	cfg := loadWith(t, "storage:\n  mailbox: maildir\n  mailbox_list_normalize_to_nfc: false\n")
	decomposed := "Café"
	got := createFolder(t, cfg.Storage, decomposed)
	if got == ".Caf\u00e9" {
		t.Error("mailbox_list_normalize_to_nfc: false had no effect — the setting is decorative")
	}
	if want := "." + decomposed; got != want {
		t.Errorf("on disk %q, want %q", got, want)
	}
}
