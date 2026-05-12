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

	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatalf("maildir.New: %v", err)
	}
	idx := file.New(dir)

	opts := imapserver.Options{
		Mailbox: mb,
		Index:   idx,
		Auth:    &stubPassdb{user: "user@test.com", pass: "testpass"},
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

// startTestServerForUser starts a server, logs in as the given user+pass and
// returns the authenticated client together with the temp dir used as root.
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

	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatalf("maildir.New: %v", err)
	}
	idx := file.New(dir)

	srv := imapserver.New(imapserver.Options{
		Mailbox: mb,
		Index:   idx,
		Auth:    &stubPassdb{user: "user@test.com", pass: "testpass"},
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

			// Each goroutine uses a distinct user by appending the goroutine index.
			user := fmt.Sprintf("user%d@test.com", g)
			pass := "testpass"

			// Stub passdb accepts any password for these synthetic users, so we
			// create per-goroutine server with its own stub.
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

			// This goroutine's user won't authenticate with the shared stub passdb
			// (which only accepts user@test.com), so authenticate as the shared user
			// and use a unique mailbox name per goroutine to avoid conflicts.
			if loginErr := c.Login("user@test.com", pass).Wait(); loginErr != nil {
				errCh <- fmt.Errorf("goroutine %d login: %w", g, loginErr)
				return
			}
			_ = user

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
