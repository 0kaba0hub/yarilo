package backendapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// fakeFTS records calls and scripts Status for the backend-api tests.
type fakeFTS struct {
	mu       sync.Mutex
	status   uint32
	rescans  []string
	optimize int
}

func (f *fakeFTS) Index(string, fts.MailboxRef, uint32, int) error { return nil }
func (f *fakeFTS) Prepend(string, fts.MailboxRef, uint32) error    { return nil }
func (f *fakeFTS) Expunge(string, fts.MailboxRef, uint32) error    { return nil }
func (f *fakeFTS) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, nil
}
func (f *fakeFTS) Status(_ string, _ fts.MailboxRef) (uint32, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, 7, nil
}
func (f *fakeFTS) Rescan(_ string, m fts.MailboxRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescans = append(f.rescans, m.Name)
	return nil
}
func (f *fakeFTS) Optimize(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.optimize++
	return nil
}
func (f *fakeFTS) Close() error { return nil }

func ftsTestServer(t *testing.T) (*httptest.Server, *fakeFTS, string) {
	t.Helper()
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	fake := &fakeFTS{status: 42}
	s := New(Options{
		Dicts:    map[string]dict.Dict{"metadata": d},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: true, Inbox: true},
		},
		MetadataDict: d,
		FTSClient:    fake,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, fake, root
}

func TestFTSStatusEndpoint(t *testing.T) {
	ts, _, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	status, body := doJSON(t, ts, http.MethodGet,
		"/api/backend/fts/status?user=alice@example.com&folder=INBOX", "", nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r struct {
		LastIndexedUID   uint32 `json:"last_indexed_uid"`
		SettingsChecksum uint32 `json:"settings_checksum"`
	}
	decodeJSONBody(t, body, &r)
	if r.LastIndexedUID != 42 || r.SettingsChecksum != 7 {
		t.Fatalf("status = %+v, want {42, 7}", r)
	}
}

func TestFTSRescanAllFolders(t *testing.T) {
	ts, fake, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()
	if err := uc.box.Create("Archive"); err != nil {
		t.Fatal(err)
	}

	status, body := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/rescan?user=alice@example.com", "", nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var r struct {
		Folders []string `json:"folders"`
	}
	decodeJSONBody(t, body, &r)
	if len(r.Folders) < 2 {
		t.Fatalf("rescanned folders = %v, want INBOX + Archive", r.Folders)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.rescans) != len(r.Folders) {
		t.Fatalf("service rescans = %v, response = %v", fake.rescans, r.Folders)
	}
}

func TestFTSRescanSingleFolder(t *testing.T) {
	ts, fake, root := ftsTestServer(t)
	uc, err := newAdminUserContext(t, ts, root, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/rescan?user=alice@example.com&folder=INBOX", "", nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.rescans) != 1 || fake.rescans[0] != "INBOX" {
		t.Fatalf("rescans = %v, want [INBOX]", fake.rescans)
	}
}

func TestFTSOptimizeEndpoint(t *testing.T) {
	ts, fake, _ := ftsTestServer(t)
	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/optimize?user=alice@example.com", "", nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.optimize != 1 {
		t.Fatalf("optimize calls = %d, want 1", fake.optimize)
	}
}

func TestFTSDisabledReturns501(t *testing.T) {
	root := t.TempDir()
	d, _ := dict.Open(dict.Config{Driver: "memory"})
	t.Cleanup(func() { _ = d.Close() })
	s := New(Options{
		Dicts:        map[string]dict.Dict{"metadata": d},
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		MetadataDict: d,
		// FTSClient deliberately nil.
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	status, _ := doJSON(t, ts, http.MethodPost,
		"/api/backend/fts/optimize?user=alice@example.com", "", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d, want 501", status)
	}
}
