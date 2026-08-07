package backendapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestACL_SetGetListDeleteRoundTrip exercises the admin ACL
// endpoints against an on-disk storage stack.
func TestACL_SetGetListDeleteRoundTrip(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	// Trigger init so the user home + INBOX dir exist.
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// SET an ACL on INBOX granting bob lr.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"acl": []map[string]any{
			{"identifier": "bob@example.com", "rights": "lr"},
			{"identifier": "anyone", "rights": "l"},
		},
	})
	if status != 200 {
		t.Fatalf("set status=%d body=%s", status, body)
	}

	// GET returns the parsed ACL.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
	})
	var getResp struct {
		Folder string `json:"folder"`
		ACL    []struct {
			Identifier string `json:"identifier"`
			Rights     string `json:"rights"`
			Negative   bool   `json:"negative"`
		} `json:"acl"`
	}
	decodeJSONBody(t, body, &getResp)
	if getResp.Folder != "INBOX" || len(getResp.ACL) != 2 {
		t.Fatalf("get response = %+v, want INBOX with 2 entries", getResp)
	}
	// On-disk canonical sort: anyone before user= by Identifier.Type ordering.
	if getResp.ACL[0].Identifier != "anyone" || getResp.ACL[0].Rights != "l" {
		t.Errorf("first entry = %+v, want anyone l", getResp.ACL[0])
	}
	if getResp.ACL[1].Identifier != "user=bob@example.com" || getResp.ACL[1].Rights != "lr" {
		t.Errorf("second entry = %+v, want user=bob@example.com lr", getResp.ACL[1])
	}

	// LIST returns the namespace-wide index entries we just wrote.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/list", "", map[string]any{
		"user": user,
	})
	var listResp struct {
		Entries []struct {
			Mailbox    string `json:"mailbox"`
			Identifier string `json:"identifier"`
			Rights     string `json:"rights"`
		} `json:"entries"`
	}
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Entries) != 2 {
		t.Fatalf("list entries = %d, want 2: %+v", len(listResp.Entries), listResp.Entries)
	}
	for _, e := range listResp.Entries {
		if e.Mailbox != "INBOX" {
			t.Errorf("unexpected mailbox %q", e.Mailbox)
		}
	}

	// DELETE drops both the file and the index entries.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/delete", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
	})
	if status != 200 {
		t.Fatalf("delete status=%d body=%s", status, body)
	}
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
	})
	decodeJSONBody(t, body, &getResp)
	if len(getResp.ACL) != 0 {
		t.Errorf("after delete, get acl = %+v, want empty", getResp.ACL)
	}
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/list", "", map[string]any{
		"user": user,
	})
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Entries) != 0 {
		t.Errorf("after delete, list entries = %+v, want empty", listResp.Entries)
	}
}

func TestACL_SetWithNegativeIdentifier(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"acl": []map[string]any{
			{"identifier": "anyone", "rights": "lr"},
			{"identifier": "-user=mallory", "rights": "lr"},
		},
	})
	if status != 200 {
		t.Fatalf("set status=%d", status)
	}

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
	})
	var getResp struct {
		ACL []struct {
			Identifier string `json:"identifier"`
			Rights     string `json:"rights"`
			Negative   bool   `json:"negative"`
		} `json:"acl"`
	}
	decodeJSONBody(t, body, &getResp)
	if len(getResp.ACL) != 2 {
		t.Fatalf("expected 2 entries, got %+v", getResp.ACL)
	}
	var sawNegative bool
	for _, e := range getResp.ACL {
		if e.Negative {
			sawNegative = true
			if e.Identifier != "-user=mallory" {
				t.Errorf("negative entry identifier = %q, want -user=mallory", e.Identifier)
			}
		}
	}
	if !sawNegative {
		t.Error("expected one negative entry to round-trip")
	}
}

func TestACL_SetRejectsInvalidRights(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"acl": []map[string]any{
			{"identifier": "anyone", "rights": "LRSZ"},
		},
	})
	if status != 400 {
		t.Errorf("expected 400 on invalid rights, got %d", status)
	}
}

func TestACL_GetMissingFolderReturnsEmpty(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
	})
	var getResp struct {
		ACL []any `json:"acl"`
	}
	decodeJSONBody(t, body, &getResp)
	if len(getResp.ACL) != 0 {
		t.Errorf("fresh mailbox should have empty ACL, got %+v", getResp.ACL)
	}
}

func TestACL_RebuildSeedsIndexFromFiles(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// Seed the per-mailbox file via SET so the index is in sync.
	doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user":   user,
		"folder": "INBOX",
		"acl":    []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
	})

	// Rebuild should re-emit the same entries.
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/acl/rebuild", "", map[string]any{
		"user":    user,
		"folders": []string{"INBOX"},
	})
	if status != 200 {
		t.Errorf("rebuild status = %d", status)
	}
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/list", "", map[string]any{
		"user": user,
	})
	var listResp struct {
		Entries []any `json:"entries"`
	}
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Entries) != 1 {
		t.Errorf("after rebuild list entries = %d, want 1", len(listResp.Entries))
	}
}

func TestACL_RequestRejectsMissingUser(t *testing.T) {
	ts, _ := storageTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"folder": "INBOX",
	})
	if status != 400 {
		t.Errorf("expected 400 on missing user, got %d", status)
	}
}

// The admin path writes the files the IMAP commands read, so a name IMAP
// refuses must not be writable through it. "/" and "." were accepted and
// stored, and on maildir both resolve to <mailroot>/../yarilo-acl — outside the
// namespace, in the directory every user's tree shares, where another
// namespace's store reads it as its own root default (#1091).
func TestACL_SetRefusesNamesIMAPWouldRefuse(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	before := treeSnapshot(t, root)

	for _, folder := range []string{".", "/", "..", "../elsewhere", "a/../b"} {
		status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
			"user":   user,
			"folder": folder,
			"acl":    []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
		})
		if status != http.StatusBadRequest {
			t.Errorf("set %q: status=%d, want 400: %s", folder, status, body)
		}
	}

	// Asserted on the tree, not the status: a handler that refused after
	// writing would pass a status-only check, and what matters here is that
	// nothing was written anywhere.
	if after := treeSnapshot(t, root); after != before {
		t.Errorf("the storage tree changed:\nbefore: %s\nafter:  %s", before, after)
	}
}

// An ordinary name still works, or the check would have disabled the command
// rather than bounded it.
func TestACL_SetStillAcceptsOrdinaryNames(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "", map[string]any{"user": user, "folder": "Sales"})

	for _, folder := range []string{"INBOX", "Sales"} {
		status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
			"user":   user,
			"folder": folder,
			"acl":    []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
		})
		if status != 200 {
			t.Errorf("set %q: status=%d: %s", folder, status, body)
		}
	}
}

func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + "\n")
		return nil
	})
	return b.String()
}

// The namespace root is grantable through the admin API — the bootstrap a
// shared namespace needs, since nobody can create its first mailbox without
// the create right and there is nowhere else to put it (#1091).
//
// Addressed by an explicit "root": true rather than by an empty folder: folder
// is required everywhere else, so a typo that dropped it would otherwise become
// a legitimate grant on the whole namespace.
func TestACL_RootIsAddressableAndNotByOmission(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// Omitting the folder is still an error, not a grant on the root.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user,
		"acl":  []map[string]any{{"identifier": "bob@example.com", "rights": "lk"}},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a missing folder: status=%d, want 400: %s", status, body)
	}

	// Asking for the root explicitly works.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user,
		"root": true,
		"acl":  []map[string]any{{"identifier": "bob@example.com", "rights": "lk"}},
	})
	if status != 200 {
		t.Fatalf("root grant: status=%d: %s", status, body)
	}

	// And sending both is refused rather than one silently winning.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user":   user,
		"root":   true,
		"folder": "INBOX",
		"acl":    []map[string]any{{"identifier": "bob@example.com", "rights": "lk"}},
	})
	if status != http.StatusBadRequest {
		t.Errorf("root plus folder: status=%d, want 400: %s", status, body)
	}
}

// An ACL may only be set on a mailbox that exists. Nothing checked here, so a
// misspelt name was not an error: the store created the directory and wrote
// the file, and the result was a mailbox with permissions and no messages
// (#1101). The IMAP side has refused since #1075; this is the same rule on the
// admin side of the wire.
//
// The assertion is on the disk as well as on the status: a 404 that still left
// the folder behind would be the same defect with a better answer.
func TestACL_MissingFolderIsRefusedAndCreatesNothing(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	const missing = "Slaes" // the typo the operator makes for "Sales"
	cases := []struct {
		name string
		path string
		body map[string]any
	}{
		{"set", "/api/backend/acl/set", map[string]any{
			"user": user, "folder": missing,
			"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lrswipkxtea"}},
		}},
		{"get", "/api/backend/acl/get", map[string]any{"user": user, "folder": missing}},
		{"delete", "/api/backend/acl/delete", map[string]any{"user": user, "folder": missing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, ts, http.MethodPost, tc.path, "", tc.body)
			if status != http.StatusNotFound {
				t.Errorf("status=%d, want 404: %s", status, body)
			}
		})
	}

	// Nothing named after the typo anywhere under the account.
	var found []string
	home := filepath.Join(root, "example.com", "alice")
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), missing) {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("the refused name left %v on disk", found)
	}

	// The mailbox that does exist is unaffected.
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user": user, "folder": "INBOX",
	}); status != 200 {
		t.Errorf("INBOX get: status=%d, want 200: %s", status, body)
	}

	// And the namespace root, which names no folder, still is not caught by
	// the existence check (#1096).
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "root": true,
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lrswipkxte"}},
	}); status != 200 {
		t.Errorf("root grant: status=%d, want 200: %s", status, body)
	}
}

// rebuild stays permissive about a name that is not there -- it reseeds from
// files on disk, creates nothing, and is the tool an operator reaches for when
// the state is already inconsistent, so refusing the batch would fail on the
// state it repairs. What it must not do is claim the work: the count was
// len(folders), so three names of which two do not exist answered
// {"folders":3,"status":"ok"} and a typo looked like a successful reseed.
func TestACL_RebuildReportsWhatItSkipped(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// INBOX carries an ACL; Sent exists with none; the other two are typos.
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "", map[string]any{
		"user": user, "folder": "Sent",
	})
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "folder": "INBOX",
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
	}); status != 200 {
		t.Fatalf("seed set: status=%d %s", status, body)
	}

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/rebuild", "", map[string]any{
		"user":    user,
		"folders": []string{"INBOX", "Sent", "Slaes", "AlsoNotThere"},
	})
	if status != 200 {
		t.Fatalf("rebuild status=%d %s", status, body)
	}
	var resp struct {
		Folders int      `json:"folders"`
		Rebuilt []string `json:"rebuilt"`
		Skipped []struct {
			Folder string `json:"folder"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	decodeJSONBody(t, body, &resp)

	if resp.Folders != 1 || len(resp.Rebuilt) != 1 || resp.Rebuilt[0] != "INBOX" {
		t.Errorf("rebuilt = %d %v, want 1 [INBOX] — the count must be the work done, not the work asked for", resp.Folders, resp.Rebuilt)
	}
	want := map[string]string{
		"Sent":         "no ACL",
		"Slaes":        "folder not found",
		"AlsoNotThere": "folder not found",
	}
	got := map[string]string{}
	for _, s := range resp.Skipped {
		got[s.Folder] = s.Reason
	}
	for folder, reason := range want {
		if got[folder] != reason {
			t.Errorf("skipped[%q] = %q, want %q", folder, got[folder], reason)
		}
	}
	if len(resp.Skipped) != len(want) {
		t.Errorf("skipped = %v, want %d entries", resp.Skipped, len(want))
	}

	// The index itself still holds only the real entry.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/list", "", map[string]any{"user": user})
	var listResp struct {
		Entries []struct {
			Mailbox string `json:"mailbox"`
		} `json:"entries"`
	}
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Entries) != 1 || listResp.Entries[0].Mailbox != "INBOX" {
		t.Errorf("index after rebuild = %+v, want the INBOX entry alone", listResp.Entries)
	}
}

// The repair path for mailboxes that predate copy-at-create: a dry run by
// default, additive only, idempotent (#1111).
func TestACL_MaterialiseIsADryRunUnlessAsked(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "", map[string]any{"user": user, "folder": "Sales"})

	// Root grants the administrator; the mailbox names only a peer — the state
	// the old rule left behind.
	doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "root": true,
		"acl": []map[string]any{{"identifier": "admin@example.com", "rights": "lrswipkxtea"}},
	})
	doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "folder": "Sales",
		"acl": []map[string]any{{"identifier": "carol@example.com", "rights": "l"}},
	})

	// Default is a dry run.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/materialise", "", map[string]any{
		"user": user, "folders": []string{"Sales"},
	})
	if status != 200 {
		t.Fatalf("materialise: status=%d %s", status, body)
	}
	var dry struct {
		Applied bool `json:"applied"`
		Added   map[string][]struct {
			Identifier string `json:"identifier"`
			Rights     string `json:"rights"`
		} `json:"added"`
	}
	decodeJSONBody(t, body, &dry)
	if dry.Applied {
		t.Error("a run without apply reported itself as applied")
	}
	if got := dry.Added["Sales"]; len(got) != 1 || got[0].Identifier != "user=admin@example.com" {
		t.Fatalf("added = %+v, want the administrator from the root", got)
	} else if got[0].Rights != "lrswipkxtea" {
		t.Errorf("added rights = %q, want the rights applying would grant", got[0].Rights)
	}
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{"user": user, "folder": "Sales"})
	if strings.Contains(string(body), "admin@example.com") {
		t.Error("the dry run wrote to disk")
	}

	// Apply, then apply again: the second run changes nothing.
	doJSON(t, ts, http.MethodPost, "/api/backend/acl/materialise", "", map[string]any{
		"user": user, "folders": []string{"Sales"}, "apply": true,
	})
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{"user": user, "folder": "Sales"})
	if !strings.Contains(string(body), "admin@example.com") {
		t.Fatalf("apply did not write: %s", body)
	}
	_, body2 := doJSON(t, ts, http.MethodPost, "/api/backend/acl/materialise", "", map[string]any{
		"user": user, "folders": []string{"Sales"}, "apply": true,
	})
	var again struct {
		Added map[string][]struct {
			Identifier string `json:"identifier"`
		} `json:"added"`
	}
	decodeJSONBody(t, body2, &again)
	if len(again.Added) != 0 {
		t.Errorf("the second run added %v", again.Added)
	}
}

// The admin path must reach a mailbox in a shared namespace. It could not: the
// namespace's UserInfo was built from Username and Home alone, so the mailbox
// backend resolved under <location>/Maildir while the ACL store resolved in
// <location>, and every per-mailbox call answered "folder not found" for a
// mailbox that was there (#1109).
//
// The assertion is that create, set and get agree about where the mailbox is —
// one path building the folder and another failing to find it is the defect,
// whatever either answers alone.
func TestACL_SharedNamespaceMailboxIsReachable(t *testing.T) {
	root := t.TempDir()
	mb := mailbox.Validating(maildir.New(), mailbox.DefaultNameRules())
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  mb,
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: true, Inbox: true},
			{Type: "shared", Prefix: "Public/", Separator: "/", List: true,
				Location: "maildir:" + filepath.Join(root, "public")},
		},
		MetadataDict: d,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// The mailbox is made the way a session makes it: the namespace location is
	// the mail root. Creating it through the admin API instead would prove
	// nothing -- with the wrong bundle both halves of the admin path are wrong
	// together and agree with each other, which is exactly how this survived.
	pub := filepath.Join(root, "public")
	sess := mb.OpenUser(&mailbox.UserInfo{
		Username: user, Home: pub, MailPath: pub, Driver: "maildir", Separator: "/",
	})
	if err := sess.Create("News"); err != nil {
		t.Fatalf("create News in the namespace location: %v", err)
	}
	_ = sess.Close()

	// The mailbox the admin path just created is a mailbox the admin path can
	// address.
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "namespace": "public", "folder": "News",
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
	}); status != 200 {
		t.Fatalf("acl set on a shared-namespace mailbox: status=%d %s", status, body)
	}
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user": user, "namespace": "public", "folder": "News",
	})
	if !strings.Contains(string(body), "bob@example.com") {
		t.Errorf("acl get on a shared-namespace mailbox: %s", body)
	}

	// And it landed beside the mail rather than in a second tree: the ACL file
	// is inside the namespace location, which is what the two consumers
	// disagreed about.
	if _, err := os.Stat(filepath.Join(root, "public", ".News", "yarilo-acl")); err != nil {
		t.Errorf("ACL file is not beside the mailbox: %v", err)
	}
}

// /acl/apply changes one entry server-side. The CLI used to GET the whole ACL,
// edit it and PUT it back through /acl/set with no lock between the two calls,
// so a concurrent write was lost and the client owned the canonical identifier
// form. apply collapses that to one call the server reads-modifies-writes under
// the folder lock (#1114).
func TestACL_ApplyChangesOneEntryServerSide(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	// The distinguishing case: an entry already on the folder, put there by a
	// path the apply caller never saw. apply is told only about carol, yet bob
	// survives -- because the server reads the current file, not a stale ACL
	// the client assembled. A get-then-set from a client whose snapshot predated
	// bob would have dropped him.
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "folder": "INBOX",
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
	}); status != 200 {
		t.Fatalf("seed bob: %d %s", status, body)
	}
	if status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/apply", "", map[string]any{
		"user": user, "folder": "INBOX",
		"identifier": "carol@example.com", "rights": "l", "mode": "replace",
	}); status != 200 {
		t.Fatalf("apply carol: %d %s", status, body)
	}
	got := aclIdentifiers(t, ts, user, "INBOX")
	if !got["user=bob@example.com"] || !got["user=carol@example.com"] {
		t.Errorf("after apply, ACL = %v; want both bob (untouched) and carol (added)", got)
	}
}

// The three modes, each read-modify-writing the one identifier.
func TestACL_ApplyModes(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	apply := func(mode, rights string) (int, []byte) {
		return doJSON(t, ts, http.MethodPost, "/api/backend/acl/apply", "", map[string]any{
			"user": user, "folder": "INBOX",
			"identifier": "bob@example.com", "rights": rights, "mode": mode,
		})
	}
	rightsOf := func() string {
		_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{"user": user, "folder": "INBOX"})
		var resp struct {
			ACL []struct {
				Identifier string `json:"identifier"`
				Rights     string `json:"rights"`
			} `json:"acl"`
		}
		decodeJSONBody(t, body, &resp)
		for _, e := range resp.ACL {
			if e.Identifier == "user=bob@example.com" {
				return e.Rights
			}
		}
		return ""
	}

	if s, b := apply("replace", "lr"); s != 200 {
		t.Fatalf("replace: %d %s", s, b)
	}
	if got := rightsOf(); got != "lr" {
		t.Errorf("after replace lr: %q", got)
	}
	if s, _ := apply("add", "sk"); s != 200 {
		t.Fatal("add")
	}
	if got := rightsOf(); got != "lrsk" {
		t.Errorf("after add sk: %q, want lrsk (canonical RFC order)", got)
	}
	if s, _ := apply("remove", "s"); s != 200 {
		t.Fatal("remove")
	}
	if got := rightsOf(); got != "lrk" {
		t.Errorf("after remove s: %q, want lrk", got)
	}
	// Replace with empty rights removes the entry (RFC 4314 §3.1).
	if s, _ := apply("replace", ""); s != 200 {
		t.Fatal("replace empty")
	}
	if got := rightsOf(); got != "" {
		t.Errorf("after replace empty: %q, want the entry gone", got)
	}
	// An unknown mode is a 400, not a silent replace.
	if s, _ := apply("toggle", "l"); s != http.StatusBadRequest {
		t.Errorf("unknown mode: status %d, want 400", s)
	}
}

// aclIdentifiers returns the set of identifiers on a folder's ACL.
func aclIdentifiers(t *testing.T, ts *httptest.Server, user, folder string) map[string]bool {
	t.Helper()
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{"user": user, "folder": folder})
	var resp struct {
		ACL []struct {
			Identifier string `json:"identifier"`
		} `json:"acl"`
	}
	decodeJSONBody(t, body, &resp)
	out := map[string]bool{}
	for _, e := range resp.ACL {
		out[e.Identifier] = true
	}
	return out
}
