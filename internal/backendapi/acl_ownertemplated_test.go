package backendapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ownerTemplatedServer wires a backend-api Server with an owner-templated shared
// namespace (user/%u/) and a userdb, so admin ACL requests exercise B1 owner
// resolution end to end.
func ownerTemplatedServer(t *testing.T, root string, udb protocol.Userdb) *httptest.Server {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: true, Inbox: true},
			{Type: "shared", Prefix: "user/%u/", Separator: "/", Location: "maildir:%h/Shared", List: true},
		},
		MetadataDict: d,
		AuthClient:   spawnAuthMaster(t, udb),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

const otSlug = "user/%u"

// The admin ACL API resolves an owner-templated mailbox to the owner named by
// the path and roots the store at the owner's userdb mail_location, not the
// namespace template (#1142). The userdb root differs from the template
// expansion, so the on-disk ACL file distinguishes the two.
func TestACL_OwnerTemplated_RoutesToOwnerUserdbStore(t *testing.T) {
	root := t.TempDir()
	userdbRoot := filepath.Join(root, "store", "alice")
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {
			Username: "alice@example.com", UID: 1001, GID: 1001,
			Home: userdbRoot, MailLocation: "maildir:" + userdbRoot,
		},
		"bob@example.com": {Username: "bob@example.com", UID: 1002, GID: 1002, Home: filepath.Join(root, "store", "bob")},
	}}
	ts := ownerTemplatedServer(t, root, udb)

	// Seed a Sent folder at the userdb root so the existence check passes.
	if err := os.MkdirAll(filepath.Join(userdbRoot, ".Sent"), 0o700); err != nil {
		t.Fatal(err)
	}

	// apply grants bob lr on alice's Sent, addressed by the owner-templated name.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/apply", "", map[string]any{
		"user": "alice@example.com", "namespace": otSlug,
		"folder": "user/alice@example.com/Sent", "identifier": "bob@example.com", "rights": "lr", "mode": "add",
	})
	if status != 200 {
		t.Fatalf("apply status=%d body=%s", status, body)
	}

	// The ACL file landed under the userdb root, not the template (<home>/Shared).
	if _, err := os.Stat(filepath.Join(userdbRoot, ".Sent", "yarilo-acl")); err != nil {
		t.Errorf("ACL not written under the userdb root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userdbRoot, "Shared", ".Sent", "yarilo-acl")); err == nil {
		t.Errorf("ACL written under the template root -- root came from the template, not the userdb")
	}

	// GET (addressed the same way) returns the grant.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user": "alice@example.com", "namespace": otSlug, "folder": "user/alice@example.com/Sent",
	})
	if !strings.Contains(string(body), "bob@example.com") {
		t.Errorf("GET does not show the grant: %s", body)
	}
}

// req.User, when given, must equal the owner the mailbox names; a mismatch is a
// 400, never a silent "the path wins".
func TestACL_OwnerTemplated_UserMismatchRefused(t *testing.T) {
	root := t.TempDir()
	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {Username: "alice@example.com", Home: filepath.Join(root, "a")},
		"bob@example.com":   {Username: "bob@example.com", Home: filepath.Join(root, "b")},
	}}
	ts := ownerTemplatedServer(t, root, udb)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/apply", "", map[string]any{
		"user": "bob@example.com", "namespace": otSlug,
		"folder": "user/alice@example.com/Sent", "identifier": "carol", "rights": "lr", "mode": "add",
	})
	if status != http.StatusBadRequest {
		t.Errorf("user/owner mismatch status=%d body=%s, want 400", status, body)
	}
}

// setActor records the acting identity as the lock owner, so an operator editing
// another account's namespace does not hold the lock under the store owner.
func TestSetActor_LockOwnerIsTheActor(t *testing.T) {
	uc := &userContext{owner: "yarilo-backend-api/1/alice"}
	uc.setActor("operator-x")
	if !strings.Contains(uc.lockOwner(), "operator-x") {
		t.Errorf("lock owner = %q, want it to name the actor", uc.lockOwner())
	}
	uc.setActor("")
	if !strings.Contains(uc.lockOwner(), "operator-x") {
		t.Errorf("empty actor changed the lock owner to %q", uc.lockOwner())
	}
}
