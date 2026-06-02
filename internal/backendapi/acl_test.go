package backendapi

import (
	"net/http"
	"testing"
)

// TestACL_SetGetListDeleteRoundTrip exercises the admin ACL surface
// against an on-disk storage stack. Mirrors the special-use /
// subscriptions tests in storage_test.go.
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
