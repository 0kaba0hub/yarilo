package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestFolderCreate_HappyPath(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"

	// Bootstrap user home + INBOX so the namespace bundle exists.
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Archive"})
	if status != 200 {
		t.Fatalf("create status=%d body=%s", status, body)
	}

	_, body = doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	var listResp struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &listResp)
	if !containsString(listResp.Folders, "Archive") {
		t.Errorf("folder Archive not in list: %v", listResp.Folders)
	}
}

func TestFolderCreate_DuplicateReturnsConflict(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	// INBOX is created by maildir Init — second create should 409.
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != http.StatusConflict {
		t.Errorf("duplicate create status=%d, want 409", status)
	}
}

func TestFolderCreate_RejectsMissingFolder(t *testing.T) {
	ts, _ := storageTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": "alice@example.com"})
	if status != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 on missing folder", status)
	}
}

func TestFolderCreate_SpecialUsePersisted(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Junk", "special_use": `\Junk`})
	if status != 200 {
		t.Fatalf("create status=%d", status)
	}
	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/specialuse/get", "",
		map[string]any{"user": user, "folder": "Junk"})
	var getResp struct {
		Attr   string `json:"attr"`
		Source string `json:"source"`
	}
	decodeJSONBody(t, body, &getResp)
	if getResp.Source != "override" || getResp.Attr != `\Junk` {
		t.Errorf("specialuse after create: source=%q attr=%q", getResp.Source, getResp.Attr)
	}
}

func TestFolderDelete_HappyPath(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Temp"})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/delete", "",
		map[string]any{"user": user, "folder": "Temp"})
	if status != 200 {
		t.Errorf("delete status=%d", status)
	}

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	var listResp struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &listResp)
	if containsString(listResp.Folders, "Temp") {
		t.Errorf("folder Temp still present after delete: %v", listResp.Folders)
	}
}

func TestFolderDelete_MissingFolder404(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/delete", "",
		map[string]any{"user": user, "folder": "Ghost"})
	if status != http.StatusNotFound {
		t.Errorf("status=%d, want 404 on missing folder", status)
	}
}

func TestFolderDelete_RemovesACLState(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Shared"})

	// Seed a yarilo-acl on the new folder so we can confirm the
	// delete drops it.
	folderDir := filepath.Join(maildirHome(root, user), ".Shared")
	if err := os.WriteFile(filepath.Join(folderDir, "yarilo-acl"), []byte("user=bob lr\n"), 0o600); err != nil {
		t.Fatalf("seed acl: %v", err)
	}
	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/delete", "",
		map[string]any{"user": user, "folder": "Shared"})
	if status != 200 {
		t.Fatalf("delete status=%d", status)
	}
	if _, err := os.Stat(filepath.Join(folderDir, "yarilo-acl")); !os.IsNotExist(err) {
		t.Errorf("yarilo-acl survived folder delete: err=%v", err)
	}
}

func TestFolderRename_HappyPath(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Old"})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/rename", "",
		map[string]any{"user": user, "old_folder": "Old", "new_folder": "New"})
	if status != 200 {
		t.Errorf("rename status=%d", status)
	}

	_, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	var listResp struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &listResp)
	if containsString(listResp.Folders, "Old") {
		t.Errorf("Old still present: %v", listResp.Folders)
	}
	if !containsString(listResp.Folders, "New") {
		t.Errorf("New missing: %v", listResp.Folders)
	}
}

func TestFolderRename_RejectsINBOX(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/rename", "",
		map[string]any{"user": user, "old_folder": "INBOX", "new_folder": "Moved"})
	if status != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 for INBOX rename: body=%s", status, body)
	}
}

func TestFolderRename_DestExistsConflict(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Src"})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Dst"})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/rename", "",
		map[string]any{"user": user, "old_folder": "Src", "new_folder": "Dst"})
	if status != http.StatusConflict {
		t.Errorf("status=%d, want 409 when dest exists", status)
	}
}

func TestFolderRename_MovesACLState(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Src"})

	srcDir := filepath.Join(maildirHome(root, user), ".Src")
	if err := os.WriteFile(filepath.Join(srcDir, "yarilo-acl"), []byte("user=bob lr\n"), 0o600); err != nil {
		t.Fatalf("seed acl: %v", err)
	}

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/rename", "",
		map[string]any{"user": user, "old_folder": "Src", "new_folder": "Dst"})
	if status != 200 {
		t.Fatalf("rename status=%d", status)
	}
	dstACL := filepath.Join(maildirHome(root, user), ".Dst", "yarilo-acl")
	if _, err := os.Stat(dstACL); err != nil {
		t.Errorf("yarilo-acl missing at new path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "yarilo-acl")); !os.IsNotExist(err) {
		t.Errorf("yarilo-acl still present at old path: err=%v", err)
	}
}

func TestFolderExpunge_NoOpOnEmptyMailbox(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/expunge", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("expunge status=%d", status)
	}
	var resp struct {
		Count    int      `json:"count"`
		Expunged []uint32 `json:"expunged"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Count != 0 {
		t.Errorf("count=%d, want 0 on empty mailbox", resp.Count)
	}
}

func TestFolderExpunge_MissingFolder404(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": user})

	status, _ := doJSON(t, ts, http.MethodPost, "/api/backend/folder/expunge", "",
		map[string]any{"user": user, "folder": "Ghost"})
	if status != http.StatusNotFound {
		t.Errorf("status=%d, want 404", status)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
