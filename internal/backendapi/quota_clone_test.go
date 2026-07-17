package backendapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/memory"
)

// quotaCloneTestServer builds a backend-api with one in-memory clone dict seeded
// with a mailbox's mirrored usage.
func quotaCloneTestServer(t *testing.T, user string) *httptest.Server {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	tx, err := d.Begin(context.Background(), &dict.OpSettings{Username: user})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Set("priv/quota/storage", []byte("2048"))
	_ = tx.Set("priv/quota/messages", []byte("7"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	s := New(Options{
		Dicts:           map[string]dict.Dict{"quota_clone_redis": d, "metadata": d},
		QuotaCloneDicts: []string{"quota_clone_redis"},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestQuotaCloneList(t *testing.T) {
	ts := quotaCloneTestServer(t, "u@x.io")
	status, body := doJSON(t, ts, http.MethodGet, "/api/backend/quota/clone/list", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r struct {
		Backends []string `json:"backends"`
	}
	decodeJSONBody(t, body, &r)
	if len(r.Backends) != 1 || r.Backends[0] != "quota_clone_redis" {
		t.Errorf("backends = %v, want [quota_clone_redis]", r.Backends)
	}
}

func TestQuotaCloneGet(t *testing.T) {
	const user = "u@x.io"
	ts := quotaCloneTestServer(t, user)

	status, body := doJSON(t, ts, http.MethodGet,
		"/api/backend/quota/clone/get?backend=quota_clone_redis&user="+user, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r quotaCloneGetResponse
	decodeJSONBody(t, body, &r)
	if !r.Found || r.StorageBytes != 2048 || r.Messages != 7 {
		t.Errorf("got %+v, want found=true storage=2048 messages=7", r)
	}
}

func TestQuotaCloneGetRejectsUnlistedBackend(t *testing.T) {
	ts := quotaCloneTestServer(t, "u@x.io")
	// "metadata" is an open dict but NOT a configured clone backend.
	status, _ := doJSON(t, ts, http.MethodGet,
		"/api/backend/quota/clone/get?backend=metadata&user=u@x.io", "", nil)
	if status != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (only clone-list backends are readable)", status)
	}
}

func TestQuotaCloneGetMissingParams(t *testing.T) {
	ts := quotaCloneTestServer(t, "u@x.io")
	status, _ := doJSON(t, ts, http.MethodGet, "/api/backend/quota/clone/get?user=u@x.io", "", nil)
	if status != http.StatusBadRequest {
		t.Errorf("missing backend: status=%d, want 400", status)
	}
}

func TestQuotaCloneGetNotFound(t *testing.T) {
	// An empty clone dict (nothing mirrored yet) reports found=false.
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := New(Options{
		Dicts:           map[string]dict.Dict{"quota_clone_redis": d},
		QuotaCloneDicts: []string{"quota_clone_redis"},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	status, body := doJSON(t, ts, http.MethodGet,
		"/api/backend/quota/clone/get?backend=quota_clone_redis&user=nobody@x.io", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r quotaCloneGetResponse
	decodeJSONBody(t, body, &r)
	if r.Found {
		t.Errorf("found=true for an empty clone dict; want false")
	}
}
