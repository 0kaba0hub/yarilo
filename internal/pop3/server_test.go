package pop3

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// ---- mock auth ---------------------------------------------------------------

type mockAuth struct {
	users map[string]string // username → password
}

func (m *mockAuth) Authenticate(user, pass, _, _ string) (*protocol.AuthResponse, error) {
	if expected, ok := m.users[user]; ok && expected == pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: user}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// ---- mock mailbox ------------------------------------------------------------
// mockMailbox implements both MailboxBackend (OpenUser) and UserMailbox (all ops).
// It returns itself from OpenUser so tests need only one type.

type mockMailbox struct {
	bodies map[string][]byte // filename → raw message
}

func (m *mockMailbox) OpenUser(_ *mailbox.UserInfo) mailbox.UserMailbox { return m }
func (m *mockMailbox) Init() error                                      { return nil }
func (m *mockMailbox) Create(_ string) error                            { return nil }
func (m *mockMailbox) Delete(_ string) error                            { return nil }
func (m *mockMailbox) FolderExists(_ string) (bool, error)              { return true, nil }
func (m *mockMailbox) ListFolders() ([]string, error)                   { return []string{"INBOX"}, nil }
func (m *mockMailbox) List(_ string) ([]*mailbox.MessageMeta, error)    { return nil, nil }
func (m *mockMailbox) Remove(_, _ string) error                         { return nil }
func (m *mockMailbox) Save(_ string, _ io.Reader, _ uint32, _ int64, _ []string) (string, error) {
	return "", nil
}
func (m *mockMailbox) Fetch(_, filename string, _ bool) (io.ReadCloser, error) {
	if m.bodies != nil {
		if b, ok := m.bodies[filename]; ok {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
	}
	return nil, fmt.Errorf("not found: %s", filename)
}
func (m *mockMailbox) Rename(_, _ string) error                    { return nil }
func (m *mockMailbox) Scan(_ string) ([]mailbox.ScanRecord, error) { return nil, nil }
func (m *mockMailbox) Close() error                                { return nil }

// ---- mock index --------------------------------------------------------------
// mockIndex implements both IndexBackend (OpenUser) and UserIndex (all ops).

type mockIndex struct {
	msgs       []*mailbox.MessageMeta
	savedUIDLs map[uint32]string
}

func (m *mockIndex) OpenUser(_ *mailbox.UserInfo) mailbox.UserIndex { return m }
func (m *mockIndex) OpenFolder(folder string, uv uint32) (*mailbox.Folder, error) {
	return &mailbox.Folder{ID: 1, Name: folder, UIDValidity: uv}, nil
}
func (m *mockIndex) SaveFolder(_ *mailbox.Folder) error                   { return nil }
func (m *mockIndex) AppendMessage(_ uint64, _ *mailbox.MessageMeta) error { return nil }
func (m *mockIndex) AllocateUID(_ uint64) (uint32, error)                 { return 0, nil }
func (m *mockIndex) UpdateFlags(_ uint64, _ uint32, _, _ []string) error  { return nil }
func (m *mockIndex) GetMessages(_ uint64, _ mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return m.msgs, nil
}
func (m *mockIndex) ExpungeMessage(_ uint64, _ uint32) error { return nil }
func (m *mockIndex) NextModSeq(_ uint64) (uint64, error)     { return 1, nil }
func (m *mockIndex) Vanished(_ uint64, _ uint64) ([]uint32, error) {
	return nil, nil
}
func (m *mockIndex) Keywords(_ uint64) ([]string, error) { return nil, nil }
func (m *mockIndex) RenameFolder(_, _ string) error      { return nil }
func (m *mockIndex) GetPOP3UIDLs(_ uint64) (map[uint32]string, error) {
	if m.savedUIDLs != nil {
		return m.savedUIDLs, nil
	}
	return make(map[uint32]string), nil
}
func (m *mockIndex) SavePOP3UIDLs(_ uint64, uidls map[uint32]string) error {
	m.savedUIDLs = uidls
	return nil
}
func (m *mockIndex) ResetFolder(_ uint64, _ []*mailbox.MessageMeta) error { return nil }
func (m *mockIndex) OptimizeIndex(_ uint64) error                         { return nil }
func (m *mockIndex) SetAltTier(_ uint64, _ []string, _ bool) error        { return nil }
func (m *mockIndex) Close() error                                         { return nil }

// ---- test helpers -----------------------------------------------------------

func newTestOpts(auth *mockAuth, mbox mailbox.MailboxBackend, idx mailbox.IndexBackend) Options {
	return Options{
		Auth:             auth,
		Mailbox:          mbox,
		Index:            idx,
		Resolver:         &mailbox.Resolver{},
		DisablePlainAuth: false,
	}
}

// newPOP3Session starts a session and returns the client pipe end.
// The greeting "+OK ..." is already consumed.
func newPOP3Session(t *testing.T, opts Options) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, s := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	c.SetDeadline(deadline) //nolint:errcheck
	s.SetDeadline(deadline) //nolint:errcheck

	srv := New(opts)
	go srv.newSession(s).serve()

	br := bufio.NewReader(c)
	greeting, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "+OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}
	t.Cleanup(func() {
		c.Close() //nolint:errcheck
	})
	return c, br
}

func send(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "%s\r\n", line); err != nil {
		t.Fatalf("send %q: %v", line, err)
	}
}

func readline(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readline: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// readUntilDot reads multi-line response lines until ".".
func readUntilDot(t *testing.T, r *bufio.Reader) []string {
	t.Helper()
	var lines []string
	for {
		line := readline(t, r)
		if line == "." {
			break
		}
		lines = append(lines, line)
	}
	return lines
}

// login performs USER + PASS and asserts +OK.
func login(t *testing.T, c net.Conn, r *bufio.Reader, user, pass string) {
	t.Helper()
	send(t, c, "USER "+user)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("USER: expected +OK, got %q", resp)
	}
	send(t, c, "PASS "+pass)
	resp = readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("PASS: expected +OK, got %q", resp)
	}
}

// ---- lock tests -------------------------------------------------------------

func TestServer_TryLock(t *testing.T) {
	srv := New(Options{})
	if !srv.tryLock("alice") {
		t.Fatal("first tryLock must succeed")
	}
	if srv.tryLock("alice") {
		t.Fatal("second tryLock on same key must fail")
	}
	srv.unlock("alice")
	if !srv.tryLock("alice") {
		t.Fatal("tryLock after unlock must succeed")
	}
	srv.unlock("alice")

	// Different keys are independent.
	if !srv.tryLock("alice") {
		t.Fatal("alice lock")
	}
	if !srv.tryLock("bob") {
		t.Fatal("bob lock must be independent")
	}
	srv.unlock("alice")
	srv.unlock("bob")
}

// ---- AUTH state tests -------------------------------------------------------

func TestSession_CAPA_AuthState(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	send(t, c, "CAPA")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("CAPA: expected +OK, got %q", resp)
	}
	caps := readUntilDot(t, r)
	capSet := make(map[string]bool)
	for _, cap := range caps {
		capSet[strings.Fields(cap)[0]] = true
	}
	for _, required := range []string{"USER", "TOP", "UIDL", "PIPELINING"} {
		if !capSet[required] {
			t.Errorf("CAPA missing %s; got: %v", required, caps)
		}
	}
}

func TestSession_CAPA_NoSTLS_WithoutTLSConfig(t *testing.T) {
	opts := newTestOpts(&mockAuth{users: map[string]string{}}, &mockMailbox{}, &mockIndex{})
	opts.TLSConfig = nil
	c, r := newPOP3Session(t, opts)

	send(t, c, "CAPA")
	readline(t, r) // +OK
	caps := readUntilDot(t, r)
	for _, cap := range caps {
		if strings.HasPrefix(cap, "STLS") {
			t.Errorf("STLS must not appear in CAPA when TLSConfig is nil, got: %v", caps)
		}
	}
}

func TestSession_UserPass_OK(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "alice", "secret")

	// Verify we're in TRANSACTION state: STAT must work.
	send(t, c, "STAT")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("STAT after login: expected +OK, got %q", resp)
	}
}

func TestSession_Pass_RequiresUser(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	send(t, c, "PASS secret")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("PASS without USER: expected -ERR, got %q", resp)
	}
}

func TestSession_Pass_WrongPassword(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "correct"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	send(t, c, "USER alice")
	readline(t, r) // +OK
	send(t, c, "PASS wrong")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("PASS with wrong password: expected -ERR, got %q", resp)
	}
}

// TestSession_AuthPlain_InitialResponse verifies RFC 5034 SASL PLAIN via the
// POP3 AUTH command with an initial response: AUTH PLAIN <base64(\0u\0p)>.
func TestSession_AuthPlain_InitialResponse(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("AUTH PLAIN with IR: expected +OK, got %q", resp)
	}
	// In TRANSACTION state — STAT must work.
	send(t, c, "STAT")
	if r2 := readline(t, r); !strings.HasPrefix(r2, "+OK") {
		t.Fatalf("STAT after AUTH PLAIN: expected +OK, got %q", r2)
	}
}

// TestSession_AuthPlain_Continuation verifies the two-step AUTH PLAIN where
// the client omits the initial response: server returns "+ ", then reads the
// base64 payload from the next line.
func TestSession_AuthPlain_Continuation(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	send(t, c, "AUTH PLAIN")
	prompt := readline(t, r)
	if !strings.HasPrefix(prompt, "+ ") && prompt != "+" {
		t.Fatalf("expected continuation prompt, got %q", prompt)
	}
	send(t, c, base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret")))
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("AUTH PLAIN continuation: expected +OK, got %q", resp)
	}
}

func TestSession_AuthPlain_WrongPassword(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "correct"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	payload := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00wrong"))
	send(t, c, "AUTH PLAIN "+payload)
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH PLAIN wrong pass: expected -ERR, got %q", resp)
	}
}

func TestSession_AuthPlain_UnsupportedMechanism(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)

	send(t, c, "AUTH CRAM-MD5")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("AUTH CRAM-MD5: expected -ERR, got %q", resp)
	}
}

func TestSession_DisablePlainAuth(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"alice": "secret"}},
		&mockMailbox{},
		&mockIndex{},
	)
	opts.DisablePlainAuth = true // requires TLS first
	c, r := newPOP3Session(t, opts)

	send(t, c, "USER alice")
	readline(t, r) // +OK
	send(t, c, "PASS secret")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("PASS without TLS when DisablePlainAuth: expected -ERR, got %q", resp)
	}
}

func TestSession_Quit_AuthState(t *testing.T) {
	opts := newTestOpts(&mockAuth{users: map[string]string{}}, &mockMailbox{}, &mockIndex{})
	c, r := newPOP3Session(t, opts)

	send(t, c, "QUIT")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT in AUTH state: expected +OK, got %q", resp)
	}
}

func TestSession_UnknownCommand_AuthState(t *testing.T) {
	opts := newTestOpts(&mockAuth{users: map[string]string{}}, &mockMailbox{}, &mockIndex{})
	c, r := newPOP3Session(t, opts)

	send(t, c, "BOGUS")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("unknown command: expected -ERR, got %q", resp)
	}
}

// ---- TRANSACTION state tests -----------------------------------------------

func TestSession_STAT_Empty(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: nil},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "STAT")
	resp := readline(t, r)
	if resp != "+OK 0 0" {
		t.Fatalf("STAT on empty mailbox: expected '+OK 0 0', got %q", resp)
	}
}

func TestSession_STAT_WithMessages(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 100},
			{UID: 2, Filename: "msg2", Size: 200},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "STAT")
	resp := readline(t, r)
	if resp != "+OK 2 300" {
		t.Fatalf("STAT: expected '+OK 2 300', got %q", resp)
	}
}

func TestSession_LIST(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 100},
			{UID: 2, Filename: "msg2", Size: 200},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "LIST")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("LIST: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	if len(lines) != 2 {
		t.Fatalf("LIST: expected 2 entries, got %d: %v", len(lines), lines)
	}
}

func TestSession_RETR(t *testing.T) {
	body := []byte("From: a@b.com\r\n\r\nHello\r\n")
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{bodies: map[string][]byte{"msg1": body}},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: uint32(len(body))},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "RETR 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("RETR 1: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	found := false
	for _, l := range lines {
		if strings.Contains(l, "Hello") {
			found = true
		}
	}
	if !found {
		t.Errorf("RETR 1: message body not found in: %v", lines)
	}
}

func TestSession_UIDL(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 100},
			{UID: 2, Filename: "msg2", Size: 200},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "UIDL")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("UIDL: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	if len(lines) != 2 {
		t.Fatalf("UIDL: expected 2 lines, got %d", len(lines))
	}
}

func TestSession_DELE_QUIT(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 100},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "DELE 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE: expected +OK, got %q", resp)
	}

	send(t, c, "QUIT")
	resp = readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT after DELE: expected +OK, got %q", resp)
	}
}

func TestSession_NOOP(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "NOOP")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("NOOP: expected +OK, got %q", resp)
	}
}

func TestSession_RSET(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 100},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "DELE 1")
	readline(t, r) // +OK

	send(t, c, "RSET")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("RSET: expected +OK, got %q", resp)
	}

	// After RSET, message 1 should be undeleted.
	send(t, c, "STAT")
	resp = readline(t, r)
	if resp != "+OK 1 100" {
		t.Fatalf("STAT after RSET: expected '+OK 1 100', got %q", resp)
	}
}

func TestSession_SaveUIDL_PersistsAcrossSessions(t *testing.T) {
	idx := &mockIndex{msgs: []*mailbox.MessageMeta{
		{UID: 1, Filename: "msg1", Size: 50},
		{UID: 2, Filename: "msg2", Size: 60},
	}}
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		idx,
	)
	opts.SaveUIDL = true

	// First session: read UIDLs (none saved yet → computed from format).
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "UIDL")
	readline(t, r) // +OK
	line1 := readline(t, r)
	line2 := readline(t, r)
	readline(t, r) // dot
	parts1 := strings.Fields(line1)
	parts2 := strings.Fields(line2)
	if len(parts1) != 2 || len(parts2) != 2 {
		t.Fatalf("unexpected UIDL response: %q %q", line1, line2)
	}
	uidl1, uidl2 := parts1[1], parts2[1]

	send(t, c, "QUIT")
	readline(t, r)

	// Verify that SavePOP3UIDLs was called on the mock index.
	if len(idx.savedUIDLs) != 2 {
		t.Fatalf("expected 2 saved UIDLs, got %d", len(idx.savedUIDLs))
	}
	if idx.savedUIDLs[1] != uidl1 || idx.savedUIDLs[2] != uidl2 {
		t.Fatalf("saved UIDLs mismatch: got %v", idx.savedUIDLs)
	}

	// Second session: UIDLs must be identical (loaded from index).
	c2, r2 := newPOP3Session(t, opts)
	login(t, c2, r2, "u", "p")

	send(t, c2, "UIDL")
	readline(t, r2) // +OK
	got1 := readline(t, r2)
	got2 := readline(t, r2)
	readline(t, r2) // dot

	if !strings.HasSuffix(got1, uidl1) {
		t.Fatalf("UIDL 1 changed: want suffix %q, got %q", uidl1, got1)
	}
	if !strings.HasSuffix(got2, uidl2) {
		t.Fatalf("UIDL 2 changed: want suffix %q, got %q", uidl2, got2)
	}

	send(t, c2, "QUIT")
	readline(t, r2)
}

func TestSession_LockSession_RejectsConcurrent(t *testing.T) {
	home := t.TempDir()
	opts := Options{
		Auth:        &mockAuth{users: map[string]string{"u": "p"}},
		Mailbox:     &mockMailbox{},
		Index:       &mockIndex{},
		Resolver:    &mailbox.Resolver{Root: home},
		LockSession: true,
	}

	// First session: acquires the dotlock.
	c1, r1 := newPOP3Session(t, opts)
	login(t, c1, r1, "u", "p")

	// Second session: must be rejected while dotlock is held.
	c2, r2 := newPOP3Session(t, opts)
	send(t, c2, "USER u")
	readline(t, r2) // +OK

	send(t, c2, "PASS p")
	resp := readline(t, r2)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("expected -ERR for concurrent session, got %q", resp)
	}

	// Release the first session; a third session must now succeed.
	send(t, c1, "QUIT")
	readline(t, r1)

	c3, r3 := newPOP3Session(t, opts)
	login(t, c3, r3, "u", "p")
	send(t, c3, "QUIT")
	readline(t, r3)
}
