package backendapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// migrateServer wires an owner-templated namespace, which no longer keeps its
// own subscription file -- the case the migration exists for.
func migrateServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {Username: "alice@example.com", Home: filepath.Join(root, "alice")},
	}}
	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  maildir.New(),
		Index:    fileindex.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: true, Inbox: true},
			{Type: "shared", Prefix: "user/%u/", Separator: "/", Location: "maildir:%h/Maildir", List: true},
		},
		MetadataDict: d,
		AuthClient:   spawnAuthMaster(t, udb),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// The rows written into the owner's store before subscriptions followed the
// subscriber are folded into the owner's own file, prefixed with the visible
// name -- so the owner keeps every subscription of their own. Pruning would have
// removed them silently, which is the one outcome with no trace.
func TestSubsMigrate_FoldsIntoTheOwnersOwnFile(t *testing.T) {
	root := t.TempDir()
	ts := migrateServer(t, root)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// Seed both historical names: the current one and the pre-#1159 path form.
	store := filepath.Join(root, "alice", "Maildir")
	if err := os.MkdirAll(filepath.Join(store, "subscriptions-user"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "subscriptions-user", "%u"), []byte("Legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The current name cannot coexist with the legacy directory of the same
	// name, so the legacy form alone stands in for both here.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/migrate", "", map[string]any{
		"user": user, "namespace": "user/%u",
	})
	if status != 200 {
		t.Fatalf("dry run status=%d body=%s", status, body)
	}
	if !strings.Contains(string(body), "user/alice@example.com/Legacy") {
		t.Errorf("dry run does not report the folded key: %s", body)
	}
	// Dry run changed nothing.
	if _, err := os.Stat(filepath.Join(store, "subscriptions-user")); err != nil {
		t.Errorf("dry run removed the source: %v", err)
	}

	// Apply.
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/migrate", "", map[string]any{
		"user": user, "namespace": "user/%u", "apply": true,
	}); status != 200 {
		t.Fatalf("apply status=%d body=%s", status, body)
	}
	personal, err := os.ReadFile(filepath.Join(root, "alice", "subscriptions"))
	if err != nil {
		t.Fatalf("personal file: %v", err)
	}
	if !strings.Contains(string(personal), "user/alice@example.com/Legacy") {
		t.Errorf("row not folded into the owner's own file: %q", personal)
	}
	if _, err := os.Stat(filepath.Join(store, "subscriptions-user")); !os.IsNotExist(err) {
		t.Errorf("source left behind after apply: %v", err)
	}
	// Idempotent: nothing left to fold.
	_, body2 := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/migrate", "", map[string]any{
		"user": user, "namespace": "user/%u", "apply": true,
	})
	if strings.Contains(string(body2), "Legacy") {
		t.Errorf("second run folded again: %s", body2)
	}
}

// A namespace that keeps its own subscriptions has nothing to migrate, and
// saying so beats folding a site-wide list into one person's file.
func TestSubsMigrate_RefusesANamespaceThatKeepsItsOwn(t *testing.T) {
	root := t.TempDir()
	ts := migrateServer(t, root)
	if status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/migrate", "", map[string]any{
		"user": "alice@example.com", "namespace": "personal",
	}); status != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for a namespace that keeps its own subscriptions", status)
	}
}
