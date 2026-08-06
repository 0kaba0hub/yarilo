package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
