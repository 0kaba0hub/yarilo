package imap_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/fts/language"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fakeFTS scripts the client side of the FTS service for session tests.
type fakeFTS struct {
	mu        sync.Mutex
	lookup    fts.Result
	lookupErr error
	lastUID   uint32
	prepends  int
	expunges  []uint32
	indexes   []uint32
	queries   []fts.Query
	// stuck models a broken FTS backend that never advances its checkpoint, even
	// after a PREPEND — the #629 failure mode.
	stuck bool
}

func (f *fakeFTS) Index(_ string, _ fts.MailboxRef, maxUID uint32, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexes = append(f.indexes, maxUID)
	return nil
}

func (f *fakeFTS) Prepend(_ string, _ fts.MailboxRef, maxUID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepends++
	if !f.stuck {
		f.lastUID = maxUID // catch-up completes on the next Status poll
	}
	return nil
}

func (f *fakeFTS) Expunge(_ string, _ fts.MailboxRef, uid uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expunges = append(f.expunges, uid)
	return nil
}

func (f *fakeFTS) Lookup(_ string, _ fts.MailboxRef, q fts.Query) (fts.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.lookup, f.lookupErr
}

func (f *fakeFTS) Status(string, fts.MailboxRef) (uint32, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUID, 1, nil
}

func (f *fakeFTS) Rescan(string, fts.MailboxRef) error { return nil }
func (f *fakeFTS) Optimize(string) error               { return nil }
func (f *fakeFTS) Close() error                        { return nil }

func startFTSTestServer(t *testing.T, fake *fakeFTS, autoindex bool) *imapclient.Client {
	t.Helper()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
		FTS: imapserver.FTSOptions{
			Client:        fake,
			Chain:         chain,
			AddMissing:    "body-search-only",
			ReadFallback:  true,
			Timeout:       3 * time.Second,
			Autoindex:     autoindex,
			SearchEnabled: true,
		},
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

func appendBody(t *testing.T, c *imapclient.Client, body string) {
	t.Helper()
	raw := "From: s@x\r\nTo: user@test.com\r\nSubject: m\r\n\r\n" + body + "\r\n"
	ac := c.Append("INBOX", int64(len(raw)), nil)
	if _, err := ac.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
}

func searchBody(t *testing.T, c *imapclient.Client, term string) []imap.UID {
	t.Helper()
	data, err := c.UIDSearch(&imap.SearchCriteria{Body: []string{term}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return data.AllUIDs()
}

// TestSearchStopwordExpansion (#722) proves a Body criterion whose every
// token is a stopword matches NOTHING (stopwords were never indexed), not
// the whole mailbox — including when it's ANDed alongside a criterion that
// DOES expand to real terms, mirroring the reference implementation's
// per-arg empty-expansion → match-nothing semantics. The fake's canned
// Lookup result is deliberately "matches everything" so a regression back
// to the old covered=allUIDs bug would make these cases pass by accident;
// asserting fake.Lookup was never called closes that gap.
func TestSearchStopwordExpansion(t *testing.T) {
	tests := []struct {
		name       string
		body       []string
		lookup     fts.Result
		wantUIDs   []imap.UID
		wantLookup bool
	}{
		{
			// "Matches everything" on purpose: if the empty-expansion bug
			// regresses, this canned result would leak through instead of
			// the constraint being caught before Lookup is ever called.
			name:       "stopword-only matches nothing",
			body:       []string{"the"},
			lookup:     fts.Result{Definite: []uint32{1, 2, 3}},
			wantUIDs:   nil,
			wantLookup: false,
		},
		{
			name:       "mixed AND with a stopword still matches nothing",
			body:       []string{"the", "report"},
			lookup:     fts.Result{Definite: []uint32{1, 2, 3}},
			wantUIDs:   nil,
			wantLookup: false,
		},
		{
			name:       "non-stopword query uses FTS normally",
			body:       []string{"report"},
			lookup:     fts.Result{Definite: []uint32{2}},
			wantUIDs:   []imap.UID{2},
			wantLookup: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeFTS{lookup: tc.lookup, lastUID: 100}
			c := startFTSTestServer(t, fake, false)
			for i := 1; i <= 3; i++ {
				appendBody(t, c, fmt.Sprintf("report payload %d", i))
			}
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := c.UIDSearch(&imap.SearchCriteria{Body: tc.body}, nil).Wait()
			if err != nil {
				t.Fatal(err)
			}
			uids := data.AllUIDs()
			if len(uids) != len(tc.wantUIDs) {
				t.Fatalf("SEARCH BODY %v = %v, want %v", tc.body, uids, tc.wantUIDs)
			}
			for i, want := range tc.wantUIDs {
				if uids[i] != want {
					t.Fatalf("SEARCH BODY %v = %v, want %v", tc.body, uids, tc.wantUIDs)
				}
			}
			fake.mu.Lock()
			called := len(fake.queries) > 0
			fake.mu.Unlock()
			if called != tc.wantLookup {
				t.Fatalf("fake.Lookup called = %v, want %v", called, tc.wantLookup)
			}
		})
	}
}

// TestSearchHeaderExpansionNotStemmed (#723, search-side counterpart of
// #696) proves a HEADER search value is expanded through the no-stemming
// data chain, not the configured language chain: buildmail indexes header
// values with lowercase-only normalization, so a stemmed query variant
// (e.g. "running" -> "run") would become a wildcard that false-positive
// matches an unrelated indexed word sharing the same stem prefix
// ("runway"). The unstemmed lowercase form must still be present so the
// query actually matches what was indexed.
func TestSearchHeaderExpansionNotStemmed(t *testing.T) {
	fake := &fakeFTS{lookup: fts.Result{Definite: []uint32{1}}, lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "irrelevant body text")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	_, err := c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: "running"}},
	}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) != 1 || len(fake.queries[0].Terms) != 1 {
		t.Fatalf("unexpected FTS queries: %+v", fake.queries)
	}
	term := fake.queries[0].Terms[0]
	if term.Field != fts.FieldHeader {
		t.Fatalf("term field = %v, want FieldHeader", term.Field)
	}
	foundUnstemmed := false
	for _, w := range term.Words {
		for _, v := range w.Variants {
			if v == "run" {
				t.Fatalf("header query wrongly stemmed %q to a %q variant — would false-positive match e.g. \"runway\": %+v", "running", v, term.Words)
			}
			if v == "running" {
				foundUnstemmed = true
			}
		}
	}
	if !foundUnstemmed {
		t.Fatalf("expected an unstemmed %q variant in the header query: %+v", "running", term.Words)
	}
}

func TestSearchUsesFTSCandidates(t *testing.T) {
	// All three messages contain the term, but the (authoritative) FTS
	// lookup returns only UID 2 — proving the scan was not used.
	fake := &fakeFTS{lookup: fts.Result{Definite: []uint32{2}}, lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	for i := 0; i < 3; i++ {
		appendBody(t, c, fmt.Sprintf("target payload %d", i))
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uids := searchBody(t, c, "target")
	if len(uids) != 1 || uids[0] != 2 {
		t.Fatalf("SEARCH BODY = %v, want [2]", uids)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) != 1 || len(fake.queries[0].Terms) != 1 ||
		fake.queries[0].Terms[0].Field != fts.FieldBody {
		t.Fatalf("unexpected FTS queries: %+v", fake.queries)
	}
}

func TestSearchVerifiesMaybe(t *testing.T) {
	// Maybe candidates are re-verified against the raw message: UID 1 does
	// not actually contain the term and must be filtered out.
	fake := &fakeFTS{lookup: fts.Result{Maybe: []uint32{1, 2}}, lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "innocent text")
	appendBody(t, c, "hidden gemstone here")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uids := searchBody(t, c, "gemstone")
	if len(uids) != 1 || uids[0] != 2 {
		t.Fatalf("maybe verification = %v, want [2]", uids)
	}
}

func TestSearchFallbackOnFTSError(t *testing.T) {
	fake := &fakeFTS{lookupErr: fmt.Errorf("boom"), lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "resilient content")
	appendBody(t, c, "other stuff")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// ReadFallback=true: the sequential scan answers correctly.
	uids := searchBody(t, c, "resilient")
	if len(uids) != 1 || uids[0] != 1 {
		t.Fatalf("fallback scan = %v, want [1]", uids)
	}
}

// TestSearchFallsBackWhenFTSStuck: a broken FTS backend that never advances its
// checkpoint must not make SEARCH hang until the full catch-up timeout (#629).
// The session detects no index progress and falls back to a sequential scan,
// which still finds the message — and it happens well before the 3s timeout.
func TestSearchFallsBackWhenFTSStuck(t *testing.T) {
	fake := &fakeFTS{stuck: true}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "stuckneedle")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	uids := searchBody(t, c, "stuckneedle")
	elapsed := time.Since(start)
	if len(uids) != 1 || uids[0] != 1 {
		t.Fatalf("fallback search = %v, want [1]", uids)
	}
	// No-progress early-exit fires at ~2s (8×250ms); a regression to waiting the
	// full 3s timeout (or hanging) is what this guards.
	if elapsed > 2800*time.Millisecond {
		t.Errorf("search took %v; the no-progress fallback should fire before the 3s timeout", elapsed)
	}
}

func TestSearchOnDemandCatchUp(t *testing.T) {
	// Index behind (lastUID=0): the session must PREPEND and poll; the fake
	// completes catch-up on Prepend.
	fake := &fakeFTS{lookup: fts.Result{Definite: []uint32{1}}}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "latecomer")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uids := searchBody(t, c, "latecomer")
	if len(uids) != 1 || uids[0] != 1 {
		t.Fatalf("catch-up search = %v, want [1]", uids)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prepends != 1 {
		t.Fatalf("prepends = %d, want 1", fake.prepends)
	}
}

func TestExpungeAndAutoindexHooks(t *testing.T) {
	fake := &fakeFTS{lastUID: 100}
	c := startFTSTestServer(t, fake, true)
	appendBody(t, c, "will vanish")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		ok := len(fake.expunges) == 1 && fake.expunges[0] == 1 && len(fake.indexes) >= 1
		fake.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	t.Fatalf("hooks not fired: expunges=%v indexes=%v", fake.expunges, fake.indexes)
}

func TestSearchFlagsIntersect(t *testing.T) {
	// FTS candidates intersect with non-FTS criteria (flags).
	fake := &fakeFTS{lookup: fts.Result{Definite: []uint32{1, 2}}, lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "shared token one")
	appendBody(t, c, "shared token two")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(2),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{
		Body: []string{"shared"},
		Flag: []imap.Flag{imap.FlagFlagged},
	}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	uids := data.AllUIDs()
	if len(uids) != 1 || uids[0] != 2 {
		t.Fatalf("intersect = %v, want [2]", uids)
	}
}

// TestSearchRelevancy (#668): SEARCH RETURN (RELEVANCY) surfaces the FTS
// engine's native scores, min-max normalized to 1-100 in ALL's enumeration
// order — verified against the reference implementation's own formula.
func TestSearchRelevancy(t *testing.T) {
	fake := &fakeFTS{
		lookup: fts.Result{
			Definite: []uint32{1, 2, 3},
			Scores: []fts.Score{
				{UID: 1, Value: 2.0},  // lowest raw score → floor 1
				{UID: 2, Value: 6.0},  // midpoint
				{UID: 3, Value: 10.0}, // highest raw score → 100
			},
		},
		lastUID: 100,
	}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "relevancy needle one")
	appendBody(t, c, "relevancy needle two")
	appendBody(t, c, "relevancy needle three")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Body: []string{"needle"}},
		&imap.SearchOptions{ReturnAll: true, ReturnRelevancy: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	uids := data.AllUIDs()
	if len(uids) != 3 {
		t.Fatalf("SEARCH hits = %v, want 3 UIDs", uids)
	}
	want := []uint32{1, 50, 100}
	if len(data.Relevancy) != 3 {
		t.Fatalf("Relevancy = %v, want len 3", data.Relevancy)
	}
	for i, w := range want {
		if data.Relevancy[i] != w {
			t.Errorf("Relevancy[%d] (uid %d) = %d, want %d", i, uids[i], data.Relevancy[i], w)
		}
	}
}

// TestSearchNoRelevancyWithoutScores: a search that never engaged FTS
// scoring (sequential-scan fallback) must omit RELEVANCY entirely rather
// than fabricate scores.
func TestSearchNoRelevancyWithoutScores(t *testing.T) {
	fake := &fakeFTS{lookupErr: fmt.Errorf("boom"), lastUID: 100}
	c := startFTSTestServer(t, fake, false)
	appendBody(t, c, "resilient content")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Body: []string{"resilient"}},
		&imap.SearchOptions{ReturnAll: true, ReturnRelevancy: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Relevancy) != 0 {
		t.Errorf("Relevancy = %v, want empty (fallback scan has no scores)", data.Relevancy)
	}
}

// TestSearchDisabledFallsBackToScanWithoutTouchingFTS (#726 item 3) proves
// fts_search=false degrades SEARCH to the sequential scan — as if FTS
// weren't configured at all — without ever calling into the FTS client,
// while leaving indexing/autoindex untouched (this test only exercises the
// SEARCH path; Index/Prepend/Expunge don't check SearchEnabled at all, by
// inspection of prepareFTSSearch's sole use of enabled()).
func TestSearchDisabledFallsBackToScanWithoutTouchingFTS(t *testing.T) {
	fake := &fakeFTS{lookup: fts.Result{Definite: []uint32{1, 2, 3}}, lastUID: 100}
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
		FTS: imapserver.FTSOptions{
			Client:        fake,
			Chain:         chain,
			AddMissing:    "body-search-only",
			ReadFallback:  true,
			Timeout:       3 * time.Second,
			SearchEnabled: false,
		},
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}

	appendBody(t, c, "onlyword payload")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uids := searchBody(t, c, "onlyword")
	if len(uids) != 1 || uids[0] != 1 {
		t.Fatalf("SEARCH BODY with fts_search=false = %v, want [1] via the raw scan", uids)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) != 0 {
		t.Fatalf("FTS Lookup called %d times, want 0 — fts_search=false must bypass FTS entirely", len(fake.queries))
	}
}
