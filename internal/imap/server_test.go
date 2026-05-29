package imap_test

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// stubPassdb accepts exactly one user/password pair.
type stubPassdb struct {
	user string
	pass string
}

func (s *stubPassdb) Authenticate(username, password, _ string) (*protocol.AuthResponse, error) {
	if username == s.user && password == s.pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// startTestServer starts an IMAP server on a random local port and returns a
// connected, un-authenticated imapclient.Client.
func startTestServer(t *testing.T) *imapclient.Client {
	t.Helper()

	dir := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}

	opts := imapserver.Options{
		Mailbox:  mb,
		Index:    idx,
		Resolver: resolver,
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
		SpecialUseDefaults: map[string]string{
			"Sent":    `\Sent`,
			"Drafts":  `\Drafts`,
			"Trash":   `\Trash`,
			"Junk":    `\Junk`,
			"Archive": `\Archive`,
		},
	}
	srv := imapserver.New(opts)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	go srv.Serve(ln) //nolint:errcheck

	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	return c
}

// startAuthClient starts a server, logs in as the given user+pass and
// returns the authenticated client.
func startAuthClient(t *testing.T, user, pass string) *imapclient.Client {
	t.Helper()
	c := startTestServer(t)
	if err := c.Login(user, pass).Wait(); err != nil {
		t.Fatalf("Login(%q): %v", user, err)
	}
	return c
}

// appendMsg appends a raw RFC 5322 message to a mailbox and returns the literal.
const testMsg = "From: sender@example.com\r\nTo: user@test.com\r\nSubject: hello\r\n\r\nHello World\r\n"

func appendMsg(t *testing.T, c *imapclient.Client, mbox string) {
	t.Helper()
	body := []byte(testMsg)
	ac := c.Append(mbox, int64(len(body)), nil)
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("Append write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("Append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("Append wait: %v", err)
	}
}

// ---- TestLogin ---------------------------------------------------------------

func TestLogin(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		pass    string
		wantErr bool
	}{
		{"valid credentials", "user@test.com", "testpass", false},
		{"wrong password", "user@test.com", "wrong", true},
		{"unknown user", "other@test.com", "testpass", true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := startTestServer(t)
			defer func() { c.Logout().Wait() }() //nolint:errcheck
			err := c.Login(tc.user, tc.pass).Wait()
			if tc.wantErr && err == nil {
				t.Fatalf("expected LOGIN error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected LOGIN error: %v", err)
			}
		})
	}
}

// ---- TestSelect ---------------------------------------------------------------

func TestSelect(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	t.Run("INBOX exists and is empty", func(t *testing.T) {
		data, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("SELECT INBOX: %v", err)
		}
		if data.NumMessages != 0 {
			t.Fatalf("expected 0 messages, got %d", data.NumMessages)
		}
	})

	t.Run("nonexistent mailbox returns NO", func(t *testing.T) {
		_, err := c.Select("NoSuchBox", nil).Wait()
		if err == nil {
			t.Fatal("expected error selecting nonexistent mailbox")
		}
	})
}

// ---- TestCreate ---------------------------------------------------------------

func TestCreate(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatalf("CREATE Work: %v", err)
	}

	if _, err := c.Select("Work", nil).Wait(); err != nil {
		t.Fatalf("SELECT Work after CREATE: %v", err)
	}
}

// ---- TestDelete ---------------------------------------------------------------

func TestDelete(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Trash", nil).Wait(); err != nil {
		t.Fatalf("CREATE Trash: %v", err)
	}
	if err := c.Delete("Trash").Wait(); err != nil {
		t.Fatalf("DELETE Trash: %v", err)
	}

	_, err := c.Select("Trash", nil).Wait()
	if err == nil {
		t.Fatal("expected SELECT Trash to fail after DELETE")
	}
}

// ---- TestAppendFetch ----------------------------------------------------------

func TestAppendFetch(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	appendMsg(t, c, "INBOX")

	data, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}
	if data.NumMessages != 1 {
		t.Fatalf("expected 1 message after APPEND, got %d", data.NumMessages)
	}

	msgs, err := c.Fetch(
		imap.SeqSetNum(1),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}},
	).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 fetch result, got %d", len(msgs))
	}
	if len(msgs[0].BodySection) == 0 {
		t.Fatal("expected body section data, got none")
	}
	got := string(msgs[0].BodySection[0].Bytes)
	if !strings.Contains(got, "Hello World") {
		t.Fatalf("unexpected body content: %q", got)
	}
}

// ---- TestStore ---------------------------------------------------------------

func TestStore(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	appendMsg(t, c, "INBOX")

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}

	storeCmd := c.Store(
		imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}},
		nil,
	)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("STORE: %v", err)
	}

	msgs, err := c.Fetch(
		imap.SeqSetNum(1),
		&imap.FetchOptions{Flags: true},
	).Collect()
	if err != nil {
		t.Fatalf("FETCH FLAGS: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 fetch result, got %d", len(msgs))
	}

	var seen bool
	for _, f := range msgs[0].Flags {
		if f == imap.FlagSeen {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("\\Seen not set after STORE +FLAGS (\\Seen); flags: %v", msgs[0].Flags)
	}
}

// ---- TestExpunge -------------------------------------------------------------

func TestExpunge(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	appendMsg(t, c, "INBOX")

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}

	storeCmd := c.Store(
		imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
		nil,
	)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("STORE +FLAGS (\\Deleted): %v", err)
	}

	if err := c.Expunge().Close(); err != nil {
		t.Fatalf("EXPUNGE: %v", err)
	}

	data, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("SELECT INBOX after EXPUNGE: %v", err)
	}
	if data.NumMessages != 0 {
		t.Fatalf("expected 0 messages after EXPUNGE, got %d", data.NumMessages)
	}
}

// ---- TestList ----------------------------------------------------------------

func TestList(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	items, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}

	var found bool
	for _, item := range items {
		if strings.EqualFold(item.Mailbox, "INBOX") {
			found = true
		}
	}
	if !found {
		t.Fatalf("INBOX not in LIST result; got: %v", items)
	}
}

// ---- TestConcurrentSessions --------------------------------------------------

func TestConcurrentSessions(t *testing.T) {
	const (
		goroutines = 3
		msgsEach   = 10
	)

	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()

	srv := imapserver.New(imapserver.Options{
		Mailbox:  mb,
		Index:    idx,
		Resolver: resolver,
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().String()

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, dialErr := net.Dial("tcp", addr)
			if dialErr != nil {
				errCh <- fmt.Errorf("goroutine %d dial: %w", g, dialErr)
				return
			}
			defer conn.Close()

			c := imapclient.New(conn, nil)
			if wgErr := c.WaitGreeting(); wgErr != nil {
				errCh <- fmt.Errorf("goroutine %d greeting: %w", g, wgErr)
				return
			}

			// Authenticate as the shared user — each goroutine uses a unique folder.
			if loginErr := c.Login("user@test.com", "testpass").Wait(); loginErr != nil {
				errCh <- fmt.Errorf("goroutine %d login: %w", g, loginErr)
				return
			}

			mbox := fmt.Sprintf("Box%d", g)
			if createErr := c.Create(mbox, nil).Wait(); createErr != nil {
				errCh <- fmt.Errorf("goroutine %d CREATE %s: %w", g, mbox, createErr)
				return
			}

			body := []byte(testMsg)
			for i := 0; i < msgsEach; i++ {
				ac := c.Append(mbox, int64(len(body)), nil)
				if _, writeErr := ac.Write(body); writeErr != nil {
					errCh <- fmt.Errorf("goroutine %d append write %d: %w", g, i, writeErr)
					return
				}
				if closeErr := ac.Close(); closeErr != nil {
					errCh <- fmt.Errorf("goroutine %d append close %d: %w", g, i, closeErr)
					return
				}
				if _, waitErr := ac.Wait(); waitErr != nil {
					errCh <- fmt.Errorf("goroutine %d append wait %d: %w", g, i, waitErr)
					return
				}
			}

			data, selErr := c.Select(mbox, nil).Wait()
			if selErr != nil {
				errCh <- fmt.Errorf("goroutine %d SELECT %s: %w", g, mbox, selErr)
				return
			}
			if data.NumMessages != uint32(msgsEach) {
				errCh <- fmt.Errorf("goroutine %d: expected %d messages, got %d", g, msgsEach, data.NumMessages)
				return
			}

			c.Logout().Wait() //nolint:errcheck
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

// ---- TestKeywords -----------------------------------------------------------

func TestKeywords(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// APPEND with a user-defined keyword.
	body := []byte(testMsg)
	flags := []imap.Flag{imap.Flag("$Forwarded")}
	ac := c.Append("INBOX", int64(len(body)), &imap.AppendOptions{Flags: flags})
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("Append write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("Append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("Append wait: %v", err)
	}

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	// FETCH FLAGS — must include $Forwarded.
	msgs, err := c.Fetch(
		imap.SeqSetNum(1),
		&imap.FetchOptions{Flags: true},
	).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 fetch result, got %d", len(msgs))
	}
	var found bool
	for _, f := range msgs[0].Flags {
		if f == "$Forwarded" {
			found = true
		}
	}
	if !found {
		t.Fatalf("$Forwarded not in FLAGS after APPEND: %v", msgs[0].Flags)
	}

	// STORE +FLAGS ($NotJunk) — add keyword via STORE.
	storeCmd := c.Store(
		imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"$NotJunk"}},
		nil,
	)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("STORE +FLAGS ($NotJunk): %v", err)
	}

	msgs2, err := c.Fetch(
		imap.SeqSetNum(1),
		&imap.FetchOptions{Flags: true},
	).Collect()
	if err != nil {
		t.Fatalf("FETCH after STORE: %v", err)
	}
	if len(msgs2) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs2))
	}
	var foundForwarded, foundNotJunk bool
	for _, f := range msgs2[0].Flags {
		switch f {
		case "$Forwarded":
			foundForwarded = true
		case "$NotJunk":
			foundNotJunk = true
		}
	}
	if !foundForwarded {
		t.Errorf("$Forwarded missing after STORE: %v", msgs2[0].Flags)
	}
	if !foundNotJunk {
		t.Errorf("$NotJunk missing after STORE: %v", msgs2[0].Flags)
	}
}

func TestSearch(t *testing.T) {
	cases := []struct {
		name     string
		criteria *imap.SearchCriteria
		wantUIDs []uint32
	}{
		{
			name:     "SEEN",
			criteria: &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}},
			wantUIDs: []uint32{1, 3},
		},
		{
			name:     "UNSEEN",
			criteria: &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}},
			wantUIDs: []uint32{2},
		},
		{
			name:     "FLAGGED",
			criteria: &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}},
			wantUIDs: []uint32{3},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := startAuthClient(t, "user@test.com", "testpass")
			defer func() { c.Logout().Wait() }() //nolint:errcheck

			body := []byte(testMsg)
			// UID 1: \Seen
			appendWithFlags(t, c, "INBOX", body, imap.FlagSeen)
			// UID 2: no flags
			appendWithFlags(t, c, "INBOX", body)
			// UID 3: \Seen \Flagged
			appendWithFlags(t, c, "INBOX", body, imap.FlagSeen, imap.FlagFlagged)

			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("SELECT: %v", err)
			}

			data, err := c.UIDSearch(tc.criteria, nil).Wait()
			if err != nil {
				t.Fatalf("UID SEARCH: %v", err)
			}
			got := data.AllUIDs()
			want := make([]imap.UID, len(tc.wantUIDs))
			for i, u := range tc.wantUIDs {
				want[i] = imap.UID(u)
			}
			if !uidSetsEqual(got, want) {
				t.Errorf("SEARCH %s: got UIDs %v, want %v", tc.name, got, want)
			}
		})
	}
}

func appendWithFlags(t *testing.T, c *imapclient.Client, mbox string, body []byte, flags ...imap.Flag) {
	t.Helper()
	var opts *imap.AppendOptions
	if len(flags) > 0 {
		opts = &imap.AppendOptions{Flags: flags}
	}
	ac := c.Append(mbox, int64(len(body)), opts)
	if _, err := ac.Write(body); err != nil {
		t.Fatalf("Append write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("Append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("Append wait: %v", err)
	}
}

func uidSetsEqual(a, b []imap.UID) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[imap.UID]bool, len(b))
	for _, u := range b {
		m[u] = true
	}
	for _, u := range a {
		if !m[u] {
			return false
		}
	}
	return true
}

// ---- TestESearchAndStatusSize (Phase IMAP-A) --------------------------------

func TestSearchESearchReturnOptions(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	body := []byte(testMsg)
	for i := 0; i < 5; i++ {
		appendWithFlags(t, c, "INBOX", body)
	}
	// Tag UIDs 2 and 4 as Seen so the criteria is non-trivial.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	for _, uid := range []uint32{2, 4} {
		us := imap.UIDSet{}
		us.AddNum(imap.UID(uid))
		if err := c.Store(us, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
			t.Fatalf("STORE seen: %v", err)
		}
	}

	t.Run("RETURN_COUNT_MIN_MAX", func(t *testing.T) {
		opts := &imap.SearchOptions{ReturnMin: true, ReturnMax: true, ReturnCount: true}
		data, err := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, opts).Wait()
		if err != nil {
			t.Fatalf("ESEARCH: %v", err)
		}
		if data.Count != 2 {
			t.Errorf("Count: got %d, want 2", data.Count)
		}
		if data.Min != 2 {
			t.Errorf("Min: got %d, want 2", data.Min)
		}
		if data.Max != 4 {
			t.Errorf("Max: got %d, want 4", data.Max)
		}
		// RETURN was given without ALL, so .All should be empty.
		if data.All != nil && len(data.AllUIDs()) > 0 {
			t.Errorf("All should be empty when RETURN omits ALL; got %v", data.AllUIDs())
		}
	})

	// NOTE on RETURN SAVE / $: server-side support is implemented
	// (substituteSearchRes + savedSearchUIDs in session). go-imap/v2
	// v2.0.0-beta.8's imapclient.returnSearchOptions omits ReturnSave from
	// the wire serialisation, so an end-to-end client test would never
	// emit `RETURN SAVE` and the server-side path stays untested via this
	// path. Re-enable when upstream ships the client fix (or via a raw
	// connection test in a follow-up).
}

func TestStatusSize(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	body := []byte(testMsg)
	for i := 0; i < 3; i++ {
		appendWithFlags(t, c, "INBOX", body)
	}

	data, err := c.Status("INBOX", &imap.StatusOptions{
		NumMessages: true,
		Size:        true,
		NumDeleted:  true,
	}).Wait()
	if err != nil {
		t.Fatalf("STATUS: %v", err)
	}
	if data.NumMessages == nil || *data.NumMessages != 3 {
		t.Errorf("NumMessages: got %v, want 3", data.NumMessages)
	}
	if data.Size == nil {
		t.Fatal("STATUS=SIZE: Size unset")
	}
	wantMin := int64(len(body)) * 3
	if *data.Size < wantMin {
		t.Errorf("Size: got %d, want >= %d", *data.Size, wantMin)
	}
	if data.NumDeleted == nil || *data.NumDeleted != 0 {
		t.Errorf("NumDeleted: got %v, want 0", data.NumDeleted)
	}
}

func TestCapabilitiesIncludeIMAP4rev2Required(t *testing.T) {
	// go-imap/v2's server advertises IMAP4rev1-extension capabilities only
	// after auth; CAPABILITY before LOGIN returns the pre-auth set.
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	wantCaps := []imap.Cap{
		imap.CapIMAP4rev2,
		imap.CapESearch,
		imap.CapSearchRes,
		imap.CapEnable,
		imap.CapSASLIR,
		imap.CapStatusSize,
	}
	for _, capName := range wantCaps {
		if _, ok := caps[capName]; !ok {
			t.Errorf("missing required IMAP4rev2 capability: %s", capName)
		}
	}
}

// ---- TestListExtendedAndSubscribe (Phase IMAP-B) ---------------------------

func TestSubscribePersistsAndAffectsListSelectSubscribed(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// Create a couple of folders so the SUBSCRIBE/UNSUBSCRIBE test has
	// targets beyond the default INBOX.
	if err := c.Create("Sent", nil).Wait(); err != nil {
		t.Fatalf("CREATE Sent: %v", err)
	}
	if err := c.Create("Drafts", nil).Wait(); err != nil {
		t.Fatalf("CREATE Drafts: %v", err)
	}

	// SUBSCRIBE INBOX + Sent. Drafts stays unsubscribed.
	if err := c.Subscribe("INBOX").Wait(); err != nil {
		t.Fatalf("SUBSCRIBE INBOX: %v", err)
	}
	if err := c.Subscribe("Sent").Wait(); err != nil {
		t.Fatalf("SUBSCRIBE Sent: %v", err)
	}

	// LIST with SELECT SUBSCRIBED filter — only the two subscribed folders.
	mailboxes, err := c.List("", "*", &imap.ListOptions{SelectSubscribed: true}).Collect()
	if err != nil {
		t.Fatalf("LIST SUBSCRIBED: %v", err)
	}
	gotSubscribed := make(map[string]bool)
	for _, mb := range mailboxes {
		gotSubscribed[mb.Mailbox] = true
	}
	for _, want := range []string{"INBOX", "Sent"} {
		if !gotSubscribed[want] {
			t.Errorf("LIST SUBSCRIBED missing %q; got %v", want, mailboxes)
		}
	}
	if gotSubscribed["Drafts"] {
		t.Errorf("LIST SUBSCRIBED should not include unsubscribed Drafts")
	}

	// UNSUBSCRIBE INBOX, verify the filter view shrinks.
	if err := c.Unsubscribe("INBOX").Wait(); err != nil {
		t.Fatalf("UNSUBSCRIBE INBOX: %v", err)
	}
	mailboxes, err = c.List("", "*", &imap.ListOptions{SelectSubscribed: true}).Collect()
	if err != nil {
		t.Fatalf("LIST SUBSCRIBED #2: %v", err)
	}
	for _, mb := range mailboxes {
		if mb.Mailbox == "INBOX" {
			t.Errorf("INBOX still listed as subscribed after UNSUBSCRIBE")
		}
	}
}

func TestListReturnSubscribedAndChildren(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatalf("CREATE Archive: %v", err)
	}
	if err := c.Subscribe("Archive").Wait(); err != nil {
		t.Fatalf("SUBSCRIBE Archive: %v", err)
	}

	mailboxes, err := c.List("", "*", &imap.ListOptions{
		ReturnSubscribed: true,
		ReturnChildren:   true,
	}).Collect()
	if err != nil {
		t.Fatalf("LIST RETURN SUBSCRIBED CHILDREN: %v", err)
	}

	var sawArchive, sawSubscribed, sawChildrenAttr bool
	for _, mb := range mailboxes {
		if mb.Mailbox != "Archive" {
			continue
		}
		sawArchive = true
		for _, attr := range mb.Attrs {
			switch attr {
			case imap.MailboxAttrSubscribed:
				sawSubscribed = true
			case imap.MailboxAttrHasChildren, imap.MailboxAttrHasNoChildren:
				// RETURN CHILDREN must yield exactly one of these two.
				// Maildir does not yet expose nested folders in
				// ListFolders, so HasNoChildren is the value here, but
				// the test asserts the broader contract.
				sawChildrenAttr = true
			}
		}
	}
	if !sawArchive {
		t.Fatalf("Archive folder not listed; got %v", mailboxes)
	}
	if !sawSubscribed {
		t.Errorf("Archive missing \\Subscribed attribute")
	}
	if !sawChildrenAttr {
		t.Errorf("Archive missing \\HasChildren / \\HasNoChildren attribute")
	}
}

// ---- TestSpecialUse (Phase IMAP-C) -----------------------------------------

func TestSpecialUseDefaultsAndCreateOverride(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// Defaults from server config (server_test setup uses the production
	// defaults map). Sent / Drafts / Trash should advertise the matching
	// special-use attr.
	for _, folder := range []string{"Sent", "Drafts", "Trash"} {
		if err := c.Create(folder, nil).Wait(); err != nil {
			t.Fatalf("CREATE %s: %v", folder, err)
		}
	}

	// CREATE with explicit USE — folder name does NOT match a default.
	if err := c.Create("CustomArchive", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive},
	}).Wait(); err != nil {
		t.Fatalf("CREATE CustomArchive USE \\Archive: %v", err)
	}

	mailboxes, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		t.Fatalf("LIST RETURN SPECIAL-USE: %v", err)
	}

	want := map[string]imap.MailboxAttr{
		"Sent":          imap.MailboxAttrSent,
		"Drafts":        imap.MailboxAttrDrafts,
		"Trash":         imap.MailboxAttrTrash,
		"CustomArchive": imap.MailboxAttrArchive,
	}
	seen := make(map[string]imap.MailboxAttr)
	for _, mb := range mailboxes {
		for _, attr := range mb.Attrs {
			switch attr {
			case imap.MailboxAttrSent, imap.MailboxAttrDrafts,
				imap.MailboxAttrTrash, imap.MailboxAttrJunk,
				imap.MailboxAttrArchive, imap.MailboxAttrAll, imap.MailboxAttrFlagged:
				seen[mb.Mailbox] = attr
			}
		}
	}
	for folder, attr := range want {
		got, ok := seen[folder]
		if !ok {
			t.Errorf("%s missing special-use attr (want %s)", folder, attr)
			continue
		}
		if got != attr {
			t.Errorf("%s: got %s, want %s", folder, got, attr)
		}
	}
}

func TestSpecialUseCapAdvertised(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	for _, capName := range []imap.Cap{imap.CapSpecialUse, imap.CapCreateSpecialUse} {
		if _, ok := caps[capName]; !ok {
			t.Errorf("missing capability: %s", capName)
		}
	}
}

// ---- TestBinaryDecode (Phase IMAP-D) ---------------------------------------

func TestFetchBinarySectionDecodesBase64(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	// base64("Hello, BINARY!\r\n") = "SGVsbG8sIEJJTkFSWSENCg=="
	raw := "From: a@b\r\n" +
		"To: c@d\r\n" +
		"Subject: bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"SGVsbG8sIEJJTkFSWSENCg==\r\n"
	appendWithFlags(t, c, "INBOX", []byte(raw))

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	seq := imap.SeqSet{}
	seq.AddNum(1)
	fcmd := c.Fetch(seq, &imap.FetchOptions{
		BinarySection: []*imap.FetchItemBinarySection{{}},
	})
	msgs, err := fcmd.Collect()
	if err != nil {
		t.Fatalf("FETCH BINARY[]: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("FETCH count: got %d, want 1", len(msgs))
	}
	if len(msgs[0].BinarySection) != 1 {
		t.Fatalf("BinarySection count: got %d, want 1", len(msgs[0].BinarySection))
	}
	got := msgs[0].BinarySection[0].Bytes
	want := "Hello, BINARY!\r\n"
	if string(got) != want {
		t.Errorf("BINARY[] body: got %q, want %q", got, want)
	}
}

func TestFetchBinarySizeMatchesDecoded(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck

	raw := "From: a@b\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"hello=20world\r\n"
	appendWithFlags(t, c, "INBOX", []byte(raw))

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	seq := imap.SeqSet{}
	seq.AddNum(1)
	msgs, err := c.Fetch(seq, &imap.FetchOptions{
		BinarySectionSize: []*imap.FetchItemBinarySectionSize{{}},
	}).Collect()
	if err != nil {
		t.Fatalf("FETCH BINARY.SIZE[]: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("FETCH count: got %d, want 1", len(msgs))
	}
	// "hello world\r\n" — 13 bytes after quoted-printable decode.
	wantSize := uint32(len("hello world\r\n"))
	if len(msgs[0].BinarySectionSize) != 1 {
		t.Fatalf("BinarySectionSize count: got %d, want 1", len(msgs[0].BinarySectionSize))
	}
	gotSize := msgs[0].BinarySectionSize[0].Size
	if gotSize != wantSize {
		t.Errorf("BINARY.SIZE[]: got %d, want %d", gotSize, wantSize)
	}
}

func TestBinaryCapAdvertised(t *testing.T) {
	c := startAuthClient(t, "user@test.com", "testpass")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("CAPABILITY: %v", err)
	}
	if _, ok := caps[imap.CapBinary]; !ok {
		t.Errorf("missing capability: %s", imap.CapBinary)
	}
}
