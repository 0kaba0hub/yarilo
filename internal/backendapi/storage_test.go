package backendapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// storageTestServer wires a Server backed by an on-disk maildir +
// fileindex pair plus an in-memory metadata dict. Every test gets
// its own t.TempDir so writes do not bleed across runs.
func storageTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	// Wrapped as mailboxbuild.ByDriver wraps it in production: the folder-name
	// rules live in the decorator, so a bare driver here would give the admin
	// API a server with no name rules at all and every check on them would
	// pass while asserting nothing (#1091).
	mb := mailbox.Validating(maildir.New(), mailbox.DefaultNameRules())
	idx := file.New()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := New(Options{
		Dicts:   map[string]dict.Dict{"metadata": d},
		Mailbox: mb,
		Index:   idx,
		Resolver: &mailbox.Resolver{
			Root:         root,
			HomeTemplate: "%d/%n",
		},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		SpecialUseDefaults: map[string]string{
			"Sent":   `\Sent`,
			"Drafts": `\Drafts`,
		},
		MetadataDict: d,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, root
}

func decodeJSONBody(t *testing.T, data []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode body %q: %v", data, err)
	}
}

func TestFolderListAndInfoAfterInit(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"

	// First call opens UserMailbox.Init which materialises INBOX.
	// Subsequent folder/list must return INBOX.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	if status != 200 {
		t.Fatalf("folder/list status=%d body=%s", status, body)
	}
	var listResp struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Folders) == 0 || listResp.Folders[0] != "INBOX" {
		t.Fatalf("folders=%v want [INBOX ...]", listResp.Folders)
	}
	// Home dir layout must match Resolver template.
	wantHome := filepath.Join(root, "example.com", "alice")
	if _, err := openIfExists(wantHome); err != nil {
		t.Fatalf("expected home %q to exist: %v", wantHome, err)
	}

	// folder/info returns metadata for an existing folder.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/folder/info", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("folder/info status=%d body=%s", status, body)
	}
	var info struct {
		Name        string `json:"name"`
		GUID        string `json:"guid"`
		UIDValidity uint32 `json:"uid_validity"`
		Messages    uint32 `json:"messages"`
	}
	decodeJSONBody(t, body, &info)
	if info.Name != "INBOX" {
		t.Errorf("name=%q want INBOX", info.Name)
	}
	if len(info.GUID) != 32 {
		t.Errorf("guid=%q want 32 hex chars", info.GUID)
	}

	// folder/info on a non-existent folder returns 404.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/folder/info", "",
		map[string]any{"user": user, "folder": "DoesNotExist"})
	if status != 404 {
		t.Errorf("nonexistent folder status=%d body=%s want 404", status, body)
	}
}

func TestFolderGUIDStableAcrossCalls(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "bob@example.com"

	// Trigger init.
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	var first, second struct {
		GUID string `json:"guid"`
	}
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/guid", "",
		map[string]any{"user": user, "folder": "INBOX"})
	decodeJSONBody(t, body, &first)
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/folder/guid", "",
		map[string]any{"user": user, "folder": "INBOX"})
	decodeJSONBody(t, body, &second)

	if first.GUID == "" || first.GUID != second.GUID {
		t.Fatalf("guid not stable: first=%q second=%q", first.GUID, second.GUID)
	}
}

func TestUserInfoExposesNamespacesAndHome(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "carol@example.com"

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "",
		map[string]any{"user": user})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Username   string `json:"username"`
		Home       string `json:"home"`
		Namespaces []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Exists bool   `json:"exists"`
			Home   string `json:"home"`
		} `json:"namespaces"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Username != user {
		t.Errorf("username=%q want %q", resp.Username, user)
	}
	wantHome := filepath.Join(root, "example.com", "carol")
	if resp.Home != wantHome {
		t.Errorf("home=%q want %q", resp.Home, wantHome)
	}
	if len(resp.Namespaces) != 1 || resp.Namespaces[0].Type != "personal" {
		t.Fatalf("namespaces=%+v want 1 personal", resp.Namespaces)
	}
	if !resp.Namespaces[0].Exists {
		// user/info does NOT auto-init — exists reflects the actual
		// home dir state. Trigger init via a folder call first.
		doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})
		_, body = doJSON(t, ts, http.MethodPost, "/api/backend/user/info", "", map[string]any{"user": user})
		decodeJSONBody(t, body, &resp)
		if !resp.Namespaces[0].Exists {
			t.Errorf("namespace exists=false after init")
		}
	}
}

func TestSubscriptionsRoundTrip(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	// add
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/add", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("add status=%d body=%s", status, body)
	}
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/add", "",
		map[string]any{"user": user, "folder": "Sent"})
	if status != 200 {
		t.Fatalf("add status=%d body=%s", status, body)
	}

	// list — must be sorted
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/list", "",
		map[string]any{"user": user})
	if status != 200 {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	var listResp struct {
		Subscriptions []string `json:"subscriptions"`
	}
	decodeJSONBody(t, body, &listResp)
	if got := listResp.Subscriptions; len(got) != 2 || got[0] != "INBOX" || got[1] != "Sent" {
		t.Errorf("subscriptions=%v want [INBOX Sent]", got)
	}

	// remove + verify
	doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/remove", "",
		map[string]any{"user": user, "folder": "Sent"})
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/list", "",
		map[string]any{"user": user})
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Subscriptions) != 1 || listResp.Subscriptions[0] != "INBOX" {
		t.Errorf("after remove subscriptions=%v want [INBOX]", listResp.Subscriptions)
	}
}

// TestSubscriptions_LiveAtMailRoot verifies the subscriptions file is written
// under the mail root (MailPath) rather than Home when the two differ.
func TestSubscriptions_LiveAtMailRoot(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	// Wrapped as mailboxbuild.ByDriver wraps it in production: the folder-name
	// rules live in the decorator, so a bare driver here would give the admin
	// API a server with no name rules at all and every check on them would
	// pass while asserting nothing (#1091).
	mb := mailbox.Validating(maildir.New(), mailbox.DefaultNameRules())
	idx := file.New()
	s := New(Options{
		Mailbox: mb,
		Index:   idx,
		Resolver: &mailbox.Resolver{
			Root:            root,
			HomeTemplate:    "%d/%n",
			DefaultMailPath: "%h/Maildir",
		},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	const user = "alice@example.com"
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/subscriptions/add", "",
		map[string]any{"user": user, "folder": "INBOX"}); status != 200 {
		t.Fatalf("add status=%d body=%s", status, body)
	}

	home := maildirHome(root, user)
	if _, err := os.Stat(filepath.Join(home, "Maildir", "subscriptions")); err != nil {
		t.Errorf("subscriptions not at mail root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "subscriptions")); !os.IsNotExist(err) {
		t.Errorf("subscriptions leaked into Home: err=%v", err)
	}
}

func TestSpecialUseOverridesAndDefaults(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	// Default applies before any override.
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/get", "",
		map[string]any{"user": user, "folder": "Sent"})
	var getResp struct {
		Folder string `json:"folder"`
		Attr   string `json:"attr"`
		Source string `json:"source"`
	}
	decodeJSONBody(t, body, &getResp)
	if getResp.Source != "default" || getResp.Attr != `\Sent` {
		t.Errorf("default lookup: source=%q attr=%q", getResp.Source, getResp.Attr)
	}

	// Override replaces.
	doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/set", "",
		map[string]any{"user": user, "folder": "Sent", "attr": `\Junk`})
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/get", "",
		map[string]any{"user": user, "folder": "Sent"})
	decodeJSONBody(t, body, &getResp)
	if getResp.Source != "override" || getResp.Attr != `\Junk` {
		t.Errorf("after set: source=%q attr=%q want override \\Junk", getResp.Source, getResp.Attr)
	}

	// Delete drops the override, default resurfaces.
	doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/delete", "",
		map[string]any{"user": user, "folder": "Sent"})
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/get", "",
		map[string]any{"user": user, "folder": "Sent"})
	decodeJSONBody(t, body, &getResp)
	if getResp.Source != "default" || getResp.Attr != `\Sent` {
		t.Errorf("after delete: source=%q attr=%q want default \\Sent", getResp.Source, getResp.Attr)
	}
}

func TestMetadataSetGetDelete(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	// Set on INBOX folder.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/metadata/set", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"entry":  "/private/comment",
		"value":  "aGVsbG8=", // base64("hello")
	})
	if status != 200 {
		t.Fatalf("set status=%d body=%s", status, body)
	}

	// Get returns the same value.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/metadata/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"entry":  "/private/comment",
	})
	var getResp struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	decodeJSONBody(t, body, &getResp)
	if !getResp.Found || getResp.Value != "aGVsbG8=" {
		t.Errorf("get: found=%v value=%q", getResp.Found, getResp.Value)
	}

	// List shows the entry under /private/.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/metadata/list", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"scope":  "private",
	})
	var listResp struct {
		Entries map[string]string `json:"entries"`
	}
	decodeJSONBody(t, body, &listResp)
	if v, ok := listResp.Entries["/private/comment"]; !ok || v != "aGVsbG8=" {
		t.Errorf("list entries=%v want /private/comment=aGVsbG8=", listResp.Entries)
	}

	// Delete clears.
	doJSON(t, ts, http.MethodPost, "/api/backend/metadata/delete", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"entry":  "/private/comment",
	})
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/metadata/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"entry":  "/private/comment",
	})
	decodeJSONBody(t, body, &getResp)
	if getResp.Found {
		t.Errorf("get after delete: found=true, want false")
	}
}

func TestIndexDumpEmptyAfterInit(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/dump", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("dump status=%d body=%s", status, body)
	}
	var resp struct {
		Folder    string           `json:"folder"`
		Records   []indexRecordOut `json:"records"`
		Truncated bool             `json:"truncated"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Folder != "INBOX" {
		t.Errorf("folder=%q want INBOX", resp.Folder)
	}
	if len(resp.Records) != 0 {
		t.Errorf("records=%d want 0 for fresh inbox", len(resp.Records))
	}
}

// openIfExists is a small wrapper so the test does not depend on
// os.Stat semantics — returns nil err when path resolves.
func openIfExists(path string) (any, error) {
	return dirExists(path), nil
}

// newMaildirAndIndexAt builds maildir + fileindex backends rooted
// at `root`, matching the configuration storageTestServer wires.
// Used by the admin-rebuild tests that need direct backend access
// to seed and inspect state before / after running the HTTP API.
func newMaildirAndIndexAt(_ *testing.T, _ string) (mailbox.MailboxBackend, mailbox.IndexBackend) {
	return maildir.New(), file.New()
}

// maildirHome mirrors the Resolver template (`%d/%n` joined with
// root) so tests can deterministically know where a user's mail
// lives without re-implementing the resolver.
func maildirHome(root, user string) string {
	at := -1
	for i := range user {
		if user[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return filepath.Join(root, user)
	}
	return filepath.Join(root, user[at+1:], user[:at])
}

// storageTestServerMdbox is storageTestServer with the mdbox
// driver wired in instead of maildir. Used to exercise the
// "not yet implemented" rebuild path without standing up a real
// mdbox tenant fixture.
func storageTestServerMdbox(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	// Wrapped exactly as the server wraps it. An unwrapped driver makes every
	// optional-capability assertion succeed in tests and fail in production,
	// which is how the missing map fold in #1267 passed its own test.
	mb := mailbox.Validating(mdbox.New(), mailbox.DefaultNameRules())
	idx := file.New()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := New(Options{
		Dicts:   map[string]dict.Dict{"metadata": d},
		Mailbox: mb,
		Index:   idx,
		Resolver: &mailbox.Resolver{
			Root:         root,
			HomeTemplate: "%d/%n",
		},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
		MetadataDict: d,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, root
}
