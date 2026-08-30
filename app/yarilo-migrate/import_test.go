package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stubWalker stands in for a source. It carries what the sources carry -- one
// flag list, system flags and keywords together -- because that is the shape
// the destination has to take apart.
type stubWalker struct {
	msgs    []sourceMessage
	folders []string
}

func (w stubWalker) Walk(_ string, visit func(sourceMessage) error) error {
	for _, m := range w.msgs {
		if err := visit(m); err != nil {
			return err
		}
	}
	return nil
}

func (w stubWalker) Folders(string) ([]string, error) { return w.folders, nil }

const importUser = "alice@example.com"

func runImport(t *testing.T, w sourceWalker, dst string) {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "example.com", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := &mailbox.Resolver{Root: dst, HomeTemplate: "%d/%n"}
	if _, _, err := migrateUser(w, src, mdbox.New(), indexfile.New(), resolver, importUser); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func openImported(t *testing.T, dst string) mailbox.UserIndex {
	t.Helper()
	idx := indexfile.New().OpenUser(&mailbox.UserInfo{
		Username: importUser,
		Home:     filepath.Join(dst, "example.com", "alice"),
	})
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// A keyword must reach the destination index. The index keeps flags and
// keywords in two fields and reads keywords from one of them only, so a source
// list handed over whole loses every keyword silently (#1561).
//
// Asserted here and not on the reader: the reader was already covered by a
// fixture test, and it passed for the whole time this was broken, because
// nothing looked at what the written index holds.
func TestAKeywordSurvivesTheImport(t *testing.T) {
	dst := t.TempDir()
	runImport(t, stubWalker{msgs: []sourceMessage{{
		Folder:       "INBOX",
		Body:         []byte("From: a@b\r\n\r\nbody\r\n"),
		Flags:        []string{`\Seen`, "$Important"},
		InternalDate: time.Unix(1700000000, 0),
	}}}, dst)

	idx := openImported(t, dst)
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open INBOX: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !hasString(msgs[0].Keywords, "$Important") {
		t.Errorf("keywords read as %v, and $Important was set on the source message", msgs[0].Keywords)
	}
	if !hasString(msgs[0].Flags, `\Seen`) {
		t.Errorf("flags read as %v, want \\Seen", msgs[0].Flags)
	}
	// The split has to be a split, not a copy: a keyword left in Flags would
	// be dropped by the system-flag mapping, and a system flag copied into
	// Keywords would come back to a client as a made-up keyword.
	if hasString(msgs[0].Keywords, `\Seen`) {
		t.Errorf("keywords read as %v, and \\Seen is a system flag", msgs[0].Keywords)
	}
	if hasString(msgs[0].Flags, "$Important") {
		t.Errorf("flags read as %v, and $Important is a keyword", msgs[0].Flags)
	}
}

// A folder holding no mail is still the user's, and it used to be created only
// under the first message that landed in it (#1563).
func TestAnEmptyFolderIsImported(t *testing.T) {
	dst := t.TempDir()
	runImport(t, stubWalker{
		folders: []string{"INBOX", "Drafts", "Trash"},
		msgs: []sourceMessage{{
			Folder: "INBOX",
			Body:   []byte("From: a@b\r\n\r\nbody\r\n"),
		}},
	}, dst)

	box := mdbox.New().OpenUser(&mailbox.UserInfo{
		Username: importUser,
		Home:     filepath.Join(dst, "example.com", "alice"),
	})
	defer box.Close() //nolint:errcheck
	entries, err := box.ListFolders()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name)
	}
	for _, want := range []string{"Drafts", "Trash"} {
		if !hasString(got, want) {
			t.Errorf("folders are %v, and %s held no mail but exists in the source", got, want)
		}
	}
}

// The import writes where the config says, because that is where the server
// reads. It used to write into the built-in layout regardless, and a store
// imported with a configured mail_index_path came up empty on every folder
// (#1562).
func TestTheImportReadsTheLayoutFromTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "yarilo.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"storage:\n  maildir_root: /unused/by/import\n  mail_index_path: \"%h/elsewhere\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := importResolver(cfgPath, "/dst", "")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if r.DefaultIndexDir != "%h/elsewhere" {
		t.Errorf("index dir is %q, and the config says %%h/elsewhere", r.DefaultIndexDir)
	}
	// --to names the root: a config's maildir_root must not send an import
	// back to the live store.
	if r.Root != "/dst" {
		t.Errorf("root is %q, want the --to value /dst", r.Root)
	}
}

// The index lands under the resolver's index dir rather than beside the mail.
func TestTheIndexLandsWhereTheResolverPointsIt(t *testing.T) {
	dst := t.TempDir()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "example.com", "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := &mailbox.Resolver{Root: dst, HomeTemplate: "%d/%n", DefaultIndexDir: "%h/elsewhere"}
	if _, _, err := migrateUser(stubWalker{msgs: []sourceMessage{{
		Folder: "INBOX",
		Body:   []byte("From: a@b\r\n\r\nbody\r\n"),
	}}}, src, mdbox.New(), indexfile.New(), resolver, importUser); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	under := filepath.Join(dst, "example.com", "alice", "elsewhere")
	found := false
	_ = filepath.Walk(under, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			found = true
		}
		return nil
	})
	if !found {
		t.Errorf("no index file under %s; the resolver's index dir was ignored", under)
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
