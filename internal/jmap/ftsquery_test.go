package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stubFTS answers lookups from a script, and records what it was asked and how
// much of it overlapped -- the fan-out's width is a claim only a counter can
// falsify.
type stubFTS struct {
	mu sync.Mutex
	// byFolder maps a folder name to what the lookup returns for it.
	byFolder map[string]fts.Result
	// err, when set, is returned for every lookup.
	err error
	// hold blocks each lookup until closed, so overlapping calls are observable.
	hold chan struct{}
	// delay per folder, to answer out of scope order.
	delay map[string]time.Duration

	// statusUID is what the index reports as indexed; prepends counts the
	// priority-index requests a lagging folder triggered. Zero means the index
	// is behind every message, so a fixture that is not about lagging must say
	// so -- searching a stale index is refused now, not answered.
	statusUID uint32
	prepends  int32

	asked    []string
	inFlight int32
	maxSeen  int32
}

func (s *stubFTS) Lookup(_ string, mbox fts.MailboxRef, _ fts.Query) (fts.Result, error) {
	n := atomic.AddInt32(&s.inFlight, 1)
	for {
		seen := atomic.LoadInt32(&s.maxSeen)
		if n <= seen || atomic.CompareAndSwapInt32(&s.maxSeen, seen, n) {
			break
		}
	}
	defer atomic.AddInt32(&s.inFlight, -1)

	s.mu.Lock()
	s.asked = append(s.asked, mbox.Name)
	d := s.delay[mbox.Name]
	s.mu.Unlock()
	if s.hold != nil {
		<-s.hold
	}
	if d > 0 {
		time.Sleep(d)
	}
	if s.err != nil {
		return fts.Result{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byFolder[mbox.Name], nil
}

func (s *stubFTS) concurrentPeak() int { return int(atomic.LoadInt32(&s.maxSeen)) }

func (s *stubFTS) Index(string, fts.MailboxRef, uint32, int) error { return nil }
func (s *stubFTS) Prepend(string, fts.MailboxRef, uint32) error {
	atomic.AddInt32(&s.prepends, 1)
	return nil
}
func (s *stubFTS) Expunge(string, fts.MailboxRef, uint32) error { return nil }
func (s *stubFTS) Rescan(string, fts.MailboxRef) error          { return nil }
func (s *stubFTS) Optimize(string) error                        { return nil }
func (s *stubFTS) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	return s.statusUID, 0, nil
}
func (s *stubFTS) Close() error { return nil }

// searchServer builds a server whose account has the named folders, each with
// one message carrying body as its text, and wires the stub as the FTS client.
func searchServer(t *testing.T, stub *stubFTS, maxConns, maxFolders int, folders map[string]string) *Server {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}

	mb := maildir.New()
	box := mb.OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	locker := &testLocker{}
	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)

	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	for _, name := range names {
		if name != "INBOX" {
			if err := box.Create(name); err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}
		raw := "Subject: probe\r\nFrom: alice@example.com\r\n\r\n" + folders[name] + "\r\n"
		fname, vsize, guid, err := box.Save(name, strings.NewReader(raw), 1, int64(len(raw)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		f, err := ui.OpenFolder(name, 0)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: 1, Filename: fname, Size: uint32(len(raw)), VSize: vsize, GUID: guid,
			InternalDate: time.Now(),
		}); err != nil {
			t.Fatalf("append %s: %v", name, err)
		}
	}
	if err := ui.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	chain, err := language.NewMultiChain([]string{"english"}, nil, nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	return New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		FTS: &FTS{
			Client: stub, Chain: chain, MaxConns: maxConns, MaxFolders: maxFolders,
		},
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
}

func folderSet(n int, body string) map[string]string {
	out := map[string]string{"INBOX": body}
	for i := 1; i < n; i++ {
		out[fmt.Sprintf("F%02d", i)] = body
	}
	return out
}

// The ceiling bounds one request's fan-out. Both sides of the boundary are
// asserted: a limit that refused everything would pass a one-sided test.
func TestEmailQueryFolderCeiling(t *testing.T) {
	const n = 4
	t.Run("at the limit the query runs", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}}
		s := searchServer(t, stub, 4, n, folderSet(n, "hello"))
		got := emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
		if got["queryState"] == nil {
			t.Errorf("query at the limit did not run: %v", got)
		}
	})

	t.Run("one folder over the limit is refused", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}}
		s := searchServer(t, stub, 4, n-1, folderSet(n, "hello"))
		err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
		if err["type"] != "invalidArguments" {
			t.Errorf("type = %v, want invalidArguments (the client can narrow inMailbox)", err["type"])
		}
		desc, _ := err["description"].(string)
		if !strings.Contains(desc, fmt.Sprint(n)) || !strings.Contains(desc, fmt.Sprint(n-1)) {
			t.Errorf("description %q names neither the count nor the limit", desc)
		}
		if len(stub.asked) != 0 {
			t.Errorf("refused query still asked the service: %v", stub.asked)
		}
	})

	// Without a text condition there is no fan-out, so the ceiling has nothing
	// to bound -- otherwise this test would be pinning the folder count.
	t.Run("a query with no text condition ignores the ceiling", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}}
		s := searchServer(t, stub, 4, 1, folderSet(n, "hello"))
		got := emailQuery(t, s, `{"accountId":"u1@example.com"}`)
		if got["queryState"] == nil {
			t.Errorf("a non-text query was refused by the full-text ceiling: %v", got)
		}
	})
}

// A Maybe is the engine saying "possibly": it must be confirmed against the
// message before it reaches a client, which is the invariant IMAP holds too.
func TestEmailQueryVerifiesMaybeResults(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantHit bool
	}{
		{"a maybe that really contains the text is kept", "the needle is here", true},
		{"a maybe that does not is dropped", "no such word", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{"INBOX": {Maybe: []uint32{1}}}}
			s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": tc.body})
			got := emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
			ids := idsOf(t, got)
			if tc.wantHit && len(ids) != 1 {
				t.Errorf("ids = %v, want the verified message", ids)
			}
			if !tc.wantHit && len(ids) != 0 {
				t.Errorf("ids = %v, want none: the candidate does not contain the text", ids)
			}
		})
	}
}

// "text" spans the address headers and the subject as well as the body (RFC
// 8621 4.4.1), and the index matched the decoded header -- so a candidate
// whose only hit is an encoded subject is a legitimate one. Confirming against
// the raw body alone would drop it, and the loss would be silent.
func TestEmailQueryVerifiesTextConditionsBeyondTheBody(t *testing.T) {
	cases := []struct {
		name    string
		headers string
		body    string
		want    string
		wantHit bool
	}{
		{"the hit is in the subject", "Subject: about the needle\r\nFrom: alice@example.com\r\n", "nothing here", "needle", true},
		{"the hit is in an encoded subject",
			"Subject: =?utf-8?B?0L3QsNC50YLQuNC90LrQsA==?=\r\nFrom: alice@example.com\r\n", "nothing here", "\u043d\u0430\u0439\u0442\u0438\u043d\u043a\u0430", true},
		{"the hit is in the from address", "Subject: hi\r\nFrom: needle@example.com\r\n", "nothing here", "needle", true},
		{"nowhere at all", "Subject: hi\r\nFrom: alice@example.com\r\n", "nothing here", "needle", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{"INBOX": {Maybe: []uint32{1}}}}
			s := rawMessageServer(t, stub, tc.headers+"\r\n"+tc.body+"\r\n")
			got := emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"`+tc.want+`"}}`)
			ids := idsOf(t, got)
			if tc.wantHit && len(ids) != 1 {
				t.Errorf("ids = %v, want the candidate confirmed", ids)
			}
			if !tc.wantHit && len(ids) != 0 {
				t.Errorf("ids = %v, want none", ids)
			}
		})
	}
}

// A Definite needs no message read, so it is answered even when the message is
// unreadable -- and that is what makes the Maybe case above about verification
// rather than about reading files.
func TestEmailQueryTrustsDefiniteResults(t *testing.T) {
	stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}}
	s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "no such word"})
	got := emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
	if ids := idsOf(t, got); len(ids) != 1 {
		t.Errorf("ids = %v, want the definite hit", ids)
	}
}

// One request takes at most half the pool, so a second request always has
// connections left rather than being refused by a queue of our own making.
func TestEmailQueryFanOutIsBounded(t *testing.T) {
	const folders = 8
	stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}, hold: make(chan struct{})}
	s := searchServer(t, stub, 4, folders, folderSet(folders, "hello"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
	}()
	// Let the first wave pile up against the hold, then release.
	time.Sleep(50 * time.Millisecond)
	close(stub.hold)
	<-done

	if peak := stub.concurrentPeak(); peak != 2 {
		t.Errorf("peak concurrent lookups = %d, want 2 (half of fts_max_conns=4)", peak)
	}
	if len(stub.asked) != folders {
		t.Errorf("asked %d folders, want %d", len(stub.asked), folders)
	}
}

// Sorting happens after the merge, so the order cannot depend on which folder
// answered first.
func TestEmailQueryOrderIndependentOfAnswerOrder(t *testing.T) {
	bodies := folderSet(3, "hello")
	stub := &stubFTS{
		statusUID: 1,
		byFolder:  map[string]fts.Result{},
		delay:     map[string]time.Duration{"INBOX": 40 * time.Millisecond},
	}
	for name := range bodies {
		stub.byFolder[name] = fts.Result{Definite: []uint32{1}}
	}
	s := searchServer(t, stub, 4, 8, bodies)

	first := idsOf(t, emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"},
		"sort":[{"property":"size"},{"property":"receivedAt"}]}`))
	stub.delay = map[string]time.Duration{"F01": 40 * time.Millisecond}
	second := idsOf(t, emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"},
		"sort":[{"property":"size"},{"property":"receivedAt"}]}`))

	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("order changed with the answer order: %v then %v", first, second)
	}
}

// The three failures a client must be able to tell apart: retry, do not retry,
// and fix the request. Only the last is the client's fault.
func TestEmailQueryFailureTypes(t *testing.T) {
	t.Run("a lookup failure is final", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}, err: errors.New("engine down")}
		s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "hello"})
		err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
		if err["type"] != "serverFail" {
			t.Errorf("type = %v, want serverFail", err["type"])
		}
	})

	// An exhausted pool is our own queue, not a broken service: telling the
	// client it is final would show an empty result as the answer.
	t.Run("an exhausted pool is transient", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}, err: ftsproto.ErrPoolExhausted}
		s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "hello"})
		err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
		if err["type"] != "serverUnavailable" {
			t.Errorf("type = %v, want serverUnavailable", err["type"])
		}
	})
}

// Without a client the text conditions are named as unsupported rather than
// answered from the index alone, which would return a confidently wrong set.
func TestEmailQueryWithoutFTSRefusesTextConditions(t *testing.T) {
	s := storedServer(t)
	err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"hello"}}`)
	if err["type"] != "unsupportedFilter" {
		t.Errorf("type = %v, want unsupportedFilter", err["type"])
	}
}

// emailQueryError runs an Email/query expected to fail and returns the method
// error object, since callAPI treats a failure as fatal.
func snippetGetError(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return methodError(t, s, "SearchSnippet/get", args)
}

func emailQueryError(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return methodError(t, s, "Email/query", args)
}

// methodError runs a call expected to fail and returns the error object, since
// callAPI treats a failure as fatal.
func methodError(t *testing.T, s *Server, method, args string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiRequest(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["`+method+`",`+args+`,"c0"]]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("response name: %v", err)
	}
	var args2 map[string]any
	if err := json.Unmarshal(resp.MethodResponses[0][1], &args2); err != nil {
		t.Fatalf("response args: %v", err)
	}
	if name != "error" {
		t.Fatalf("call succeeded, want a method error: %v", args2)
	}
	return args2
}

// A lagging index is not an empty mailbox. IMAP falls back to the exact scan
// here; Email/query cannot, so it says "retry" instead of answering from a
// half-indexed folder.
func TestEmailQueryWaitsForALaggingIndexThenAsksForARetry(t *testing.T) {
	t.Run("still behind when the budget runs out", func(t *testing.T) {
		stub := &stubFTS{byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}}
		s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "needle here"})
		s.opts.FTS.AddMissing = "priority"
		s.opts.FTS.Timeout = 300 * time.Millisecond

		err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
		if err["type"] != "serverUnavailable" {
			t.Errorf("type = %v, want serverUnavailable so the client retries", err["type"])
		}
		if atomic.LoadInt32(&stub.prepends) == 0 {
			t.Error("the lagging folder was never queued for indexing")
		}
	})

	// Without fts_search_add_missing there is nothing to queue -- but noticing
	// the lag needs no knob, and searching a folder known to be half-indexed
	// would answer with less mail than the account holds.
	t.Run("behind with no add-missing knob refuses at once", func(t *testing.T) {
		stub := &stubFTS{byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}}
		s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "needle here"})
		s.opts.FTS.AddMissing = ""

		err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
		if err["type"] != "serverUnavailable" {
			t.Errorf("type = %v, want serverUnavailable", err["type"])
		}
		if n := atomic.LoadInt32(&stub.prepends); n != 0 {
			t.Errorf("queued %d indexing requests without the knob, want 0", n)
		}
	})

	// Caught up in time: the same wiring must then answer normally, or the
	// case above would pass with a catch-up that never succeeds.
	t.Run("caught up in time", func(t *testing.T) {
		stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}}
		s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "needle here"})
		s.opts.FTS.AddMissing = "priority"
		s.opts.FTS.Timeout = 300 * time.Millisecond

		got := emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
		if ids := idsOf(t, got); len(ids) != 1 {
			t.Errorf("ids = %v, want the hit from a caught-up index", ids)
		}
	})
}

// The waiting budget belongs to the request, not to each folder: a per-folder
// one multiplies by the fan-out, so a query at the ceiling would hold the
// client and half the pool for minutes.
func TestLaggingIndexBudgetIsPerRequest(t *testing.T) {
	const folders, timeout = 8, 200 * time.Millisecond
	stub := &stubFTS{byFolder: map[string]fts.Result{}} // statusUID 0: every folder is behind
	s := searchServer(t, stub, 4, folders, folderSet(folders, "needle"))
	s.opts.FTS.AddMissing = "priority"
	s.opts.FTS.Timeout = timeout

	start := time.Now()
	err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
	elapsed := time.Since(start)

	if err["type"] != "serverUnavailable" {
		t.Errorf("type = %v, want serverUnavailable", err["type"])
	}
	// Two at a time over eight folders is four waves; a per-folder budget would
	// spend four timeouts, a shared one spends about one.
	if max := 2 * timeout; elapsed > max {
		t.Errorf("waited %v for %d lagging folders, want under %v: the budget is per folder, not per request",
			elapsed, folders, max)
	}
}

// A folder with no GUID cannot be searched: the index is keyed by it (#1183).
// Skipping such a folder would answer from part of the account while looking
// like the whole of it, so the query refuses -- and as serverFail, not
// invalidArguments: the client named nothing wrong and cannot see which folder
// to exclude.
func TestFolderWithoutGUIDRefusesRatherThanSkips(t *testing.T) {
	stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{}}
	s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "hello"})
	eval := s.newFTSEvaluator(&userHandle{info: &mailbox.UserInfo{Username: testUser}})

	text := "hello"
	err := eval.prepare(context.Background(), nil, scopeFolder{name: "Broken", id: 7}, &jmapcore.EmailFilter{Text: &text})
	var noGUID *errFolderWithoutGUID
	if !errors.As(err, &noGUID) {
		t.Fatalf("prepare on a folder with no GUID returned %v, want the refusal", err)
	}
	if len(stub.asked) != 0 {
		t.Errorf("the service was asked about a folder with no identity: %v", stub.asked)
	}

	merr := queryPrepareError("Broken", err)
	if merr.Type != jmapcore.ErrServerFail {
		t.Errorf("type = %s, want serverFail", merr.Type)
	}
	if !strings.Contains(merr.Description, "Broken") {
		t.Errorf("description %q does not name the folder", merr.Description)
	}
}

// The transient conditions map to the type that says "retry"; each is asserted
// where the mapping happens, since a query cannot easily produce both.
func TestPrepareErrorsCarryTheirRetryability(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an exhausted pool", errPoolBusy, jmapcore.ErrServerUnavailable},
		{"a lagging index", errIndexLagging, jmapcore.ErrServerUnavailable},
		{"a broken lookup", errors.New("engine down"), jmapcore.ErrServerFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := queryPrepareError("F", tc.err).Type; got != tc.want {
				t.Errorf("type = %s, want %s", got, tc.want)
			}
		})
	}
}

// rawMessageServer is searchServer with one INBOX message written verbatim, so
// a test can put the searchable text where it means to.
func rawMessageServer(t *testing.T, stub *stubFTS, raw string) *Server {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}

	mb := maildir.New()
	box := mb.OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	locker := &testLocker{}
	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)

	fname, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open INBOX: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: fname, Size: uint32(len(raw)), VSize: vsize, GUID: guid, InternalDate: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ui.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	chain, err := language.NewMultiChain([]string{"english"}, nil, nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	return New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		FTS:    &FTS{Client: stub, Chain: chain, MaxConns: 4, MaxFolders: 8},
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
}

// One folder's refusal is the whole query's answer, so the rest must stop
// instead of each taking a lock and a round trip for a result nobody will
// read. On the sandbox this walked dozens of folders and finished eleven
// seconds after the client had hung up (#1214).
func TestFanOutStopsAtTheFirstRefusal(t *testing.T) {
	const folders = 8
	stub := &stubFTS{
		statusUID: 1,
		byFolder:  map[string]fts.Result{},
		err:       errors.New("engine down"),
		delay:     map[string]time.Duration{},
	}
	// Every lookup is slow, so a run that walks them all takes visibly longer
	// than one that stops.
	for name := range folderSet(folders, "needle") {
		stub.delay[name] = 60 * time.Millisecond
	}
	s := searchServer(t, stub, 4, folders, folderSet(folders, "needle"))

	start := time.Now()
	if err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`); err["type"] != "serverFail" {
		t.Errorf("type = %v, want serverFail", err["type"])
	}
	elapsed := time.Since(start)

	asked := len(stub.asked)
	// Two at a time: without the early stop all eight are asked; with it only
	// the wave in flight when the first failed.
	if asked >= folders {
		t.Errorf("asked %d of %d folders: the fan-out did not stop at the first refusal", asked, folders)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v: the query ran on past the answer", elapsed)
	}
}

// A wedged index never advances, so waiting out the budget only guarantees the
// client is gone before the answer arrives. IMAP grew this exit in #629.
func TestCatchUpGivesUpWhenTheIndexIsNotAdvancing(t *testing.T) {
	stub := &stubFTS{byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}} // statusUID 0 forever
	s := searchServer(t, stub, 4, 8, map[string]string{"INBOX": "needle here"})
	s.opts.FTS.AddMissing = "priority"
	s.opts.FTS.Timeout = 30 * time.Second // the budget a deployment actually runs with

	start := time.Now()
	err := emailQueryError(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`)
	elapsed := time.Since(start)

	if err["type"] != "serverUnavailable" {
		t.Errorf("type = %v, want serverUnavailable", err["type"])
	}
	// ~2s of a flat checkpoint is enough; waiting the full 30s is the defect.
	if elapsed > 10*time.Second {
		t.Errorf("waited %v on an index that never moves, want the early exit", elapsed)
	}
}
