package adminapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/memory"
)

// memTestServer wires a Server with a single in-memory dict named
// "test" and the supplied auth options. Returns the httptest.Server
// and the live dict so tests can verify side-effects.
func memTestServer(t *testing.T, token string) (*httptest.Server, dict.Dict) {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := New(Options{
		Token: token,
		Dicts: map[string]dict.Dict{"test": d},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, d
}

func doJSON(t *testing.T, ts *httptest.Server, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var br io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		br = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, br)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestHealthEndpoint(t *testing.T) {
	ts, _ := memTestServer(t, "")
	resp, data := doJSON(t, ts, http.MethodGet, "/api/admin/health", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("body = %s, want status:ok", data)
	}
}

func TestAuthRejectsMissingOrWrongToken(t *testing.T) {
	ts, _ := memTestServer(t, "supersecret")

	// No header → 401
	resp, _ := doJSON(t, ts, http.MethodGet, "/api/admin/dict/drivers", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status=%d, want 401", resp.StatusCode)
	}
	// Wrong token → 401
	resp, _ = doJSON(t, ts, http.MethodGet, "/api/admin/dict/drivers", "wrong", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status=%d, want 401", resp.StatusCode)
	}
	// Correct token → 200
	resp, _ = doJSON(t, ts, http.MethodGet, "/api/admin/dict/drivers", "supersecret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("good token: status=%d, want 200", resp.StatusCode)
	}
}

func TestDictDriversListed(t *testing.T) {
	ts, _ := memTestServer(t, "")
	resp, data := doJSON(t, ts, http.MethodGet, "/api/admin/dict/drivers", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, data)
	}
	var got struct {
		Drivers []string `json:"drivers"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, d := range got.Drivers {
		if d == "memory" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("drivers list missing memory: %v", got.Drivers)
	}
}

func TestDictExistsKnown(t *testing.T) {
	ts, _ := memTestServer(t, "")
	resp, data := doJSON(t, ts, http.MethodGet, "/api/admin/dict/test/exists", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, data)
	}
	var got struct {
		Name   string `json:"name"`
		Exists bool   `json:"exists"`
	}
	_ = json.Unmarshal(data, &got)
	if !got.Exists || got.Name != "test" {
		t.Errorf("got %+v, want exists=true name=test", got)
	}
}

func TestDictExistsUnknown(t *testing.T) {
	ts, _ := memTestServer(t, "")
	_, data := doJSON(t, ts, http.MethodGet, "/api/admin/dict/no-such/exists", "", nil)
	var got struct {
		Exists bool `json:"exists"`
	}
	_ = json.Unmarshal(data, &got)
	if got.Exists {
		t.Errorf("unknown dict reported as exists: %s", data)
	}
}

func TestDictRoundtripSetLookupUnset(t *testing.T) {
	ts, _ := memTestServer(t, "")

	// SET
	resp, data := doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/set", "", map[string]any{
		"key":   "priv/foo",
		"value": []byte("hello"), // encoding/json emits base64
	})
	if resp.StatusCode != 200 {
		t.Fatalf("set status %d body %s", resp.StatusCode, data)
	}
	var setResp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &setResp)
	if setResp.Status != "ok" {
		t.Errorf("set status = %q, want ok", setResp.Status)
	}

	// LOOKUP
	resp, data = doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/lookup", "", map[string]any{"key": "priv/foo"})
	if resp.StatusCode != 200 {
		t.Fatalf("lookup status %d body %s", resp.StatusCode, data)
	}
	var lk struct {
		Found  bool     `json:"found"`
		Values [][]byte `json:"values"`
	}
	_ = json.Unmarshal(data, &lk)
	if !lk.Found || len(lk.Values) != 1 || string(lk.Values[0]) != "hello" {
		t.Errorf("lookup mismatch: %+v", lk)
	}

	// UNSET
	resp, _ = doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/unset", "", map[string]any{"key": "priv/foo"})
	if resp.StatusCode != 200 {
		t.Fatalf("unset status %d", resp.StatusCode)
	}
	// LOOKUP again — should miss
	_, data = doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/lookup", "", map[string]any{"key": "priv/foo"})
	_ = json.Unmarshal(data, &lk)
	if lk.Found {
		t.Errorf("key still found after unset: %+v", lk)
	}
}

func TestDictAtomicInc(t *testing.T) {
	ts, _ := memTestServer(t, "")

	// Seed via SET
	doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/set", "", map[string]any{
		"key":   "counter",
		"value": []byte("10"),
	})
	// Inc by 5
	resp, _ := doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/atomic-inc", "", map[string]any{
		"key":   "counter",
		"delta": 5,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("inc status %d", resp.StatusCode)
	}
	// Lookup
	_, data := doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/lookup", "", map[string]any{"key": "counter"})
	var lk struct {
		Values [][]byte `json:"values"`
	}
	_ = json.Unmarshal(data, &lk)
	if string(lk.Values[0]) != "15" {
		t.Errorf("counter = %s, want 15", lk.Values[0])
	}
}

func TestDictAtomicIncMissingKeyReturnsNotFound(t *testing.T) {
	ts, _ := memTestServer(t, "")
	_, data := doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/atomic-inc", "", map[string]any{
		"key":   "no-such",
		"delta": 1,
	})
	var resp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Status != "not-found" {
		t.Errorf("status = %q, want not-found", resp.Status)
	}
}

func TestDictIterateNDJSONStream(t *testing.T) {
	ts, d := memTestServer(t, "")
	// Seed three rows directly via dict
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Set("priv/a", []byte("v-a"))
	_ = tx.Set("priv/b", []byte("v-b"))
	_ = tx.Set("priv/c", []byte("v-c"))
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// IterateRecurse + IterateSortByKey = 1 | 2 = 3
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/dict/test/iterate", bytes.NewReader([]byte(`{"path":"priv/","flags":3}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q, want application/x-ndjson", ct)
	}
	var keys []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var row struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(sc.Bytes(), &row)
		keys = append(keys, row.Key)
	}
	want := []string{"priv/a", "priv/b", "priv/c"}
	if len(keys) != len(want) {
		t.Fatalf("got %d rows, want %d (%v)", len(keys), len(want), keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("row[%d]=%q want %q (sort-by-key broken?)", i, keys[i], want[i])
		}
	}
}

func TestDictCommitBatchAtomic(t *testing.T) {
	ts, _ := memTestServer(t, "")
	body := map[string]any{
		"ops": []map[string]any{
			{"kind": "set", "key": "a", "value": []byte("1")},
			{"kind": "set", "key": "b", "value": []byte("2")},
			{"kind": "atomic-inc", "key": "a", "delta": 10}, // 1 → 11
		},
	}
	resp, data := doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/commit-batch", "", body)
	if resp.StatusCode != 200 {
		t.Fatalf("commit-batch status %d body %s", resp.StatusCode, data)
	}
	var cr struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &cr)
	if cr.Status != "ok" {
		t.Errorf("status = %q, want ok", cr.Status)
	}
	// Verify final value
	_, data = doJSON(t, ts, http.MethodPost, "/api/admin/dict/test/lookup", "", map[string]any{"key": "a"})
	var lk struct {
		Values [][]byte `json:"values"`
	}
	_ = json.Unmarshal(data, &lk)
	if string(lk.Values[0]) != "11" {
		t.Errorf("counter = %s, want 11", lk.Values[0])
	}
}

func TestUnknownDictReturns404(t *testing.T) {
	ts, _ := memTestServer(t, "")
	resp, _ := doJSON(t, ts, http.MethodPost, "/api/admin/dict/no-such/lookup", "", map[string]string{"key": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
