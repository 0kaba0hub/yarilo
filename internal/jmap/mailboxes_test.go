package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"net"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const testUser = "u1@example.com"

// storedServer builds a server over a real maildir and index, so the counts and
// roles come from the same code path IMAP reads rather than a stub.
func storedServer(t *testing.T) *Server {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}

	mb := maildir.New()
	box := mb.OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, folder := range []string{"Sent", "Archive", "Work", "Work/Reports"} {
		if err := box.Create(folder); err != nil {
			t.Fatalf("create %s: %v", folder, err)
		}
	}

	locker := &testLocker{}
	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	// Two messages in INBOX, one of them unread, so the counters are distinct
	// and a swapped total/unread cannot pass.
	deliver(t, box, ui, "INBOX", 1, []string{`\Seen`})
	deliver(t, box, ui, "INBOX", 2, nil)

	if err := ui.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	return New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
			SpecialUseDefaults: map[string]string{
				"Sent":    `\Sent`,
				"Archive": `\Archive`,
			},
		},
	})
}

func deliver(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, folder string, uid uint32, flags []string) {
	t.Helper()
	body := "Subject: test\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Save(folder, strings.NewReader(body), uid, int64(len(body)), flags, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: uid, Filename: name, Size: uint32(len(body)), VSize: vsize, Flags: flags, GUID: guid,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func callAPI(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) == 0 {
		t.Fatalf("no method responses: %s", w.Body)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("response name: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.MethodResponses[0][1], &args); err != nil {
		t.Fatalf("response args: %v", err)
	}
	if name == "error" {
		t.Fatalf("method failed: %v", args)
	}
	return args
}

// A null ids means every mailbox (RFC 8620 §5.1). An empty array means none —
// the two must not collapse into one.
func TestMailboxGetNullIdsMeansAll(t *testing.T) {
	s := storedServer(t)
	tests := []struct {
		name, args string
		wantLen    int
	}{
		{"null ids", `{"accountId":"u1@example.com","ids":null}`, 5},
		{"absent ids", `{"accountId":"u1@example.com"}`, 5},
		{"empty ids", `{"accountId":"u1@example.com","ids":[]}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
				["Mailbox/get",`+tt.args+`,"c0"]]}`)
			list, _ := got["list"].([]any)
			if len(list) != tt.wantLen {
				t.Errorf("list has %d mailboxes, want %d", len(list), tt.wantLen)
			}
		})
	}
}

// An id that does not exist goes to notFound, not to a method error: the rest
// of the request still has to be answerable.
func TestMailboxGetReportsNotFound(t *testing.T) {
	s := storedServer(t)
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com","ids":["nope"]},"c0"]]}`)
	notFound, _ := got["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != "nope" {
		t.Errorf("notFound = %v", notFound)
	}
	if list, _ := got["list"].([]any); len(list) != 0 {
		t.Errorf("list = %v, want empty", list)
	}
}

// The role comes from the special-use layers, defaults included: reading only
// the per-user overrides would drop every role an operator configured.
func TestMailboxRolesComeFromBothSpecialUseLayers(t *testing.T) {
	s := storedServer(t)
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com"},"c0"]]}`)
	roles := map[string]any{}
	for _, item := range got["list"].([]any) {
		mb := item.(map[string]any)
		roles[mb["name"].(string)] = mb["role"]
	}
	want := map[string]any{
		"INBOX":   "inbox",   // no IMAP attribute, but the JMAP inbox role
		"Sent":    "sent",    // from the configured defaults
		"Archive": "archive", // from the configured defaults
		"Work":    nil,       // no role at all
	}
	for name, wantRole := range want {
		if roles[name] != wantRole {
			t.Errorf("role of %s = %v, want %v", name, roles[name], wantRole)
		}
	}
}

// The counters are the folder's own, and total must not be reported as unread.
func TestMailboxCountersComeFromTheIndex(t *testing.T) {
	s := storedServer(t)
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com"},"c0"]]}`)
	for _, item := range got["list"].([]any) {
		mb := item.(map[string]any)
		if mb["name"] != "INBOX" {
			continue
		}
		if mb["totalEmails"] != float64(2) {
			t.Errorf("totalEmails = %v, want 2", mb["totalEmails"])
		}
		if mb["unreadEmails"] != float64(1) {
			t.Errorf("unreadEmails = %v, want 1", mb["unreadEmails"])
		}
		// Until threads are computed each message is its own thread, so the
		// counts coincide rather than being reported as zero.
		if mb["totalThreads"] != mb["totalEmails"] {
			t.Errorf("totalThreads = %v, totalEmails = %v", mb["totalThreads"], mb["totalEmails"])
		}
		return
	}
	t.Fatal("INBOX is missing from the list")
}

// Hierarchy travels as parentId, and the name is the leaf: a client must never
// have to know the server's delimiter.
func TestMailboxHierarchyUsesParentID(t *testing.T) {
	s := storedServer(t)
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com"},"c0"]]}`)
	byName := map[string]map[string]any{}
	for _, item := range got["list"].([]any) {
		mb := item.(map[string]any)
		byName[mb["name"].(string)] = mb
	}
	reports, ok := byName["Reports"]
	if !ok {
		t.Fatalf("nested mailbox is missing; got %v", keysOf(byName))
	}
	if strings.Contains(reports["name"].(string), "/") {
		t.Errorf("name = %v, want the leaf only", reports["name"])
	}
	work := byName["Work"]
	if reports["parentId"] != work["id"] {
		t.Errorf("parentId = %v, want Work's id %v", reports["parentId"], work["id"])
	}
	if byName["INBOX"]["parentId"] != nil {
		t.Errorf("INBOX parentId = %v, want null", byName["INBOX"]["parentId"])
	}
}

// Mailbox/query filters and states plainly that it cannot calculate changes.
func TestMailboxQuery(t *testing.T) {
	s := storedServer(t)
	tests := []struct {
		name, args string
		wantIDs    int
	}{
		{"no filter", `{"accountId":"u1@example.com"}`, 5},
		{"by role", `{"accountId":"u1@example.com","filter":{"role":"inbox"}}`, 1},
		{"has any role", `{"accountId":"u1@example.com","filter":{"hasAnyRole":true}}`, 3},
		{"by name", `{"accountId":"u1@example.com","filter":{"name":"work"}}`, 1},
		{"limit", `{"accountId":"u1@example.com","limit":2}`, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
				["Mailbox/query",`+tt.args+`,"c0"]]}`)
			ids, _ := got["ids"].([]any)
			if len(ids) != tt.wantIDs {
				t.Errorf("ids = %d, want %d (%v)", len(ids), tt.wantIDs, got)
			}
			if got["canCalculateChanges"] != false {
				t.Errorf("canCalculateChanges = %v, want false", got["canCalculateChanges"])
			}
			if got["queryState"] == "" {
				t.Error("queryState is empty")
			}
		})
	}
}

// A filter or sort the server cannot honour is refused, never ignored: a client
// that asked for an order and got another one renders the wrong list.
func TestMailboxQueryRefusesWhatItCannotHonour(t *testing.T) {
	s := storedServer(t)
	tests := []struct {
		name, args, wantErr string
	}{
		{"filter operator", `{"accountId":"u1@example.com","filter":{"operator":"AND","conditions":[]}}`,
			jmapcore.ErrUnsupportedFilter},
		{"unknown sort", `{"accountId":"u1@example.com","sort":[{"property":"totalEmails"}]}`,
			jmapcore.ErrUnsupportedSort},
		{"other account", `{"accountId":"someone@else"}`, jmapcore.ErrAccountNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
				["Mailbox/query",`+tt.args+`,"c0"]]}`))
			var resp struct {
				MethodResponses [][]json.RawMessage `json:"methodResponses"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			var name string
			if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
				t.Fatalf("name: %v", err)
			}
			if name != "error" {
				t.Fatalf("response = %s, want error", name)
			}
			var merr jmapcore.MethodError
			if err := json.Unmarshal(resp.MethodResponses[0][1], &merr); err != nil {
				t.Fatalf("args: %v", err)
			}
			if merr.Type != tt.wantErr {
				t.Errorf("error type = %s, want %s", merr.Type, tt.wantErr)
			}
		})
	}
}

// The Mailbox methods need the mail capability declared, like every other
// method needs its own.
func TestMailboxMethodsNeedTheMailCapability(t *testing.T) {
	s := storedServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Mailbox/get",{"accountId":"u1@example.com"},"c0"]]}`))
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("name: %v", err)
	}
	if name != "error" {
		t.Errorf("response = %s, want error", name)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// testLocker is an in-process stand-in. It locks per resource and reports what
// it holds, because that is the contract the code depends on: the mail index
// re-enters MailboxKey while the message cache holds it, and the real client
// answers HoldsResource rather than blocking on itself.
type testLocker struct {
	mu    sync.Mutex
	holds map[string]int
	// taken counts acquisitions, so a caller that opens per message instead of
	// per folder is visible rather than merely slower.
	taken int
	// owners is every name the caller announced itself under.
	owners map[string]struct{}
}

func (l *testLocker) acquisitions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.taken
}

func (l *testLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holds == nil {
		l.holds = map[string]int{}
	}
	l.holds[resource]++
	l.taken++
	if l.owners == nil {
		l.owners = map[string]struct{}{}
	}
	l.owners[owner] = struct{}{}
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *testLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

func (l *testLocker) Unlock(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holds[id]--; l.holds[id] <= 0 {
		delete(l.holds, id)
	}
	return nil
}

func (l *testLocker) Renew(context.Context, string, time.Duration) error { return nil }

func (l *testLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return make(chan locks.Event), nil
}

func (l *testLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *testLocker) HoldsResource(resource string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holds[resource] > 0
}

func (l *testLocker) IncrementCounter(context.Context, string, int64) (int64, error) { return 0, nil }
func (l *testLocker) Close() error                                                   { return nil }
