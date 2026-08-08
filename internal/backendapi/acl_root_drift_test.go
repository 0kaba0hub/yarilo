package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// The root ACL is the row that inherits everywhere; it must be readable,
// removable and legible through the same CLI surface that writes it -- set
// could create state nothing could inspect or remove (#1163).
func TestACL_RootReadDeleteAndListingMarker(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "root": true,
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lk"}},
	})
	if status != 200 {
		t.Fatalf("root set: status=%d body=%s", status, body)
	}

	// get --root reads it back.
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user": user, "root": true,
	})
	var getResp struct {
		ACL []struct {
			Identifier string `json:"identifier"`
			Rights     string `json:"rights"`
		} `json:"acl"`
	}
	decodeJSONBody(t, body, &getResp)
	if len(getResp.ACL) != 1 || getResp.ACL[0].Identifier != "user=bob@example.com" {
		t.Fatalf("root get = %+v, want the bob entry", getResp)
	}

	// The empty mailbox is the root's one name in the listing -- no second
	// field restating it (the two-fields-about-one-fact review point).
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/list", "", map[string]any{"user": user})
	var listResp struct {
		Entries []map[string]any `json:"entries"`
	}
	decodeJSONBody(t, body, &listResp)
	if len(listResp.Entries) != 1 || listResp.Entries[0]["mailbox"] != "" {
		t.Fatalf("listing rows = %+v, want one row with the empty mailbox", listResp.Entries)
	}
	if _, dup := listResp.Entries[0]["root"]; dup {
		t.Fatalf("listing row carries a root field duplicating mailbox == \"\": %+v", listResp.Entries[0])
	}

	// Single-identifier removal via apply, DELETEACL semantics.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/apply", "", map[string]any{
		"user": user, "root": true, "identifier": "bob@example.com", "rights": "", "mode": "replace",
	})
	if status != 200 {
		t.Fatalf("root apply-delete: status=%d body=%s", status, body)
	}
	// Whole-file delete via delete --root.
	status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/delete", "", map[string]any{
		"user": user, "root": true,
	})
	if status != 200 {
		t.Fatalf("root delete: status=%d body=%s", status, body)
	}
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/get", "", map[string]any{
		"user": user, "root": true,
	})
	decodeJSONBody(t, body, &getResp)
	if len(getResp.ACL) != 0 {
		t.Errorf("after root delete, get = %+v, want empty", getResp.ACL)
	}
}

// rebuild --dry-run answers "did my deployment drift, and where" without
// presuming the folder list the drifted index was supposed to provide (#1154).
func TestACL_RebuildDryRunReportsDrift(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/acl/set", "", map[string]any{
		"user": user, "folder": "INBOX",
		"acl": []map[string]any{{"identifier": "bob@example.com", "rights": "lr"}},
	})

	// Manufacture drift: blank the index while the per-mailbox file stays.
	var listPath string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && info.Name() == "yarilo-acl-list" {
			listPath = p
		}
		return nil
	})
	if listPath == "" {
		t.Fatal("yarilo-acl-list not found after set")
	}
	if err := os.Truncate(listPath, 0); err != nil {
		t.Fatal(err)
	}

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/rebuild", "", map[string]any{
		"user": user, "all": true, "dry_run": true,
	})
	if status != 200 {
		t.Fatalf("dry-run status=%d body=%s", status, body)
	}
	var rep struct {
		DryRun bool `json:"dry_run"`
		InSync bool `json:"in_sync"`
		Drift  []struct {
			Folder  string `json:"folder"`
			Missing []struct {
				Identifier string `json:"identifier"`
				Rights     string `json:"rights"`
			} `json:"missing"`
			Stale []any `json:"stale"`
		} `json:"drift"`
	}
	decodeJSONBody(t, body, &rep)
	if !rep.DryRun || rep.InSync {
		t.Fatalf("drift not reported: %s", body)
	}
	if len(rep.Drift) != 1 || rep.Drift[0].Folder != "INBOX" ||
		len(rep.Drift[0].Missing) != 1 || rep.Drift[0].Missing[0].Identifier != "user=bob@example.com" {
		t.Fatalf("drift = %s, want INBOX missing user=bob@example.com", body)
	}

	// The dry run wrote nothing: the index is still blank.
	if fi, err := os.Stat(listPath); err != nil || fi.Size() != 0 {
		t.Fatalf("dry run touched the index (size=%v err=%v)", fi.Size(), err)
	}

	// A real rebuild repairs it; the next dry run is clean.
	if status, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/rebuild", "", map[string]any{
		"user": user, "all": true,
	}); status != 200 {
		t.Fatalf("rebuild status=%d body=%s", status, body)
	}
	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/acl/rebuild", "", map[string]any{
		"user": user, "all": true, "dry_run": true,
	})
	decodeJSONBody(t, body, &rep)
	if !rep.InSync {
		t.Fatalf("after rebuild the dry run still reports drift: %s", body)
	}
}
