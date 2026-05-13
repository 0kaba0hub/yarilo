package pop3

import (
	"bufio"
	"bytes"
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

func (m *mockAuth) Authenticate(user, pass, _ string) (*protocol.AuthResponse, error) {
	if expected, ok := m.users[user]; ok && expected == pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: user}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// ---- mock mailbox ------------------------------------------------------------

type mockMailbox struct {
	bodies map[string][]byte // filename → raw message
}

func (m *mockMailbox) Init(_ string) error                              { return nil }
func (m *mockMailbox) Create(_, _ string) error                         { return nil }
func (m *mockMailbox) Delete(_, _ string) error                         { return nil }
func (m *mockMailbox) FolderExists(_, _ string) (bool, error)           { return true, nil }
func (m *mockMailbox) ListFolders(_ string) ([]string, error)           { return []string{"INBOX"}, nil }
func (m *mockMailbox) List(_, _ string) ([]*mailbox.MessageMeta, error) { return nil, nil }
func (m *mockMailbox) Remove(_, _, _ string) error                      { return nil }
func (m *mockMailbox) Save(_ string, _ string, _ io.Reader, _ int64, _ []string) (string, error) {
	return "", nil
}
func (m *mockMailbox) Fetch(_, _, filename string) (io.ReadCloser, error) {
	if m.bodies != nil {
		if b, ok := m.bodies[filename]; ok {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
	}
	return nil, fmt.Errorf("not found: %s", filename)
}

// ---- mock index --------------------------------------------------------------

type mockIndex struct {
	msgs []*mailbox.MessageMeta
}

func (m *mockIndex) OpenFolder(_, folder string, uv uint32) (*mailbox.Folder, error) {
	return &mailbox.Folder{ID: 1, Name: folder, UIDValidity: uv}, nil
}
func (m *mockIndex) SaveFolder(_ string, _ *mailbox.Folder) error         { return nil }
func (m *mockIndex) AppendMessage(_ uint64, _ *mailbox.MessageMeta) error { return nil }
func (m *mockIndex) UpdateFlags(_ uint64, _ uint32, _, _ []string) error  { return nil }
func (m *mockIndex) GetMessages(_ uint64, _ mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return m.msgs, nil
}
func (m *mockIndex) ExpungeMessage(_ uint64, _ uint32) error { return nil }
func (m *mockIndex) NextModSeq(_ uint64) (uint64, error)     { return 1, nil }
func (m *mockIndex) Close() error                            { return nil }

// ---- test helpers -----------------------------------------------------------

func newTestOpts(auth *mockAuth, mbox mailbox.MailboxBackend, idx *mockIndex) Options {
	return Options{
		Auth:             auth,
		Mailbox:          mbox,
		Index:            idx,
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

func TestSession_LIST_Empty(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "LIST")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("LIST: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	if len(lines) != 0 {
		t.Fatalf("LIST on empty mailbox: expected 0 lines, got %v", lines)
	}
}

func TestSession_LIST_WithMessages(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 512},
			{UID: 2, Filename: "msg2", Size: 1024},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "LIST")
	readline(t, r) // +OK 2 messages...
	lines := readUntilDot(t, r)
	if len(lines) != 2 {
		t.Fatalf("LIST: expected 2 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "1 512" || lines[1] != "2 1024" {
		t.Fatalf("LIST: unexpected lines %v", lines)
	}
}

func TestSession_LIST_SingleMsg(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 7, Filename: "x", Size: 42},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "LIST 1")
	resp := readline(t, r)
	if resp != "+OK 1 42" {
		t.Fatalf("LIST 1: expected '+OK 1 42', got %q", resp)
	}
}

func TestSession_RETR(t *testing.T) {
	body := []byte("From: alice@example.com\r\nSubject: hi\r\n\r\nHello.\r\n")
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
		t.Fatalf("RETR: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	got := strings.Join(lines, "\r\n")
	if !strings.Contains(got, "Hello.") {
		t.Fatalf("RETR: body not found in response: %v", lines)
	}
}

func TestSession_RETR_NoSuchMsg(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "RETR 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "-ERR") {
		t.Fatalf("RETR nonexistent: expected -ERR, got %q", resp)
	}
}

func TestSession_DELE_and_RSET(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 10},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	// Delete message 1.
	send(t, c, "DELE 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE: expected +OK, got %q", resp)
	}

	// STAT must show 0 after deletion.
	send(t, c, "STAT")
	resp = readline(t, r)
	if resp != "+OK 0 0" {
		t.Fatalf("STAT after DELE: expected '+OK 0 0', got %q", resp)
	}

	// RSET must restore the message.
	send(t, c, "RSET")
	resp = readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("RSET: expected +OK, got %q", resp)
	}

	// STAT must show 1 again.
	send(t, c, "STAT")
	resp = readline(t, r)
	if resp != "+OK 1 10" {
		t.Fatalf("STAT after RSET: expected '+OK 1 10', got %q", resp)
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

func TestSession_UIDL_Empty(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "UIDL")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("UIDL: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	if len(lines) != 0 {
		t.Fatalf("UIDL on empty mailbox: expected 0 lines, got %v", lines)
	}
}

func TestSession_UIDL_Format(t *testing.T) {
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 5, Filename: "msg5", Size: 100},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "UIDL 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK 1 5.") {
		t.Fatalf("UIDL 1: expected '+OK 1 5.<uidvalidity>', got %q", resp)
	}
}

func TestSession_TOP(t *testing.T) {
	body := []byte("From: a@b.com\r\nSubject: test\r\n\r\nLine1\r\nLine2\r\nLine3\r\n")
	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		&mockMailbox{bodies: map[string][]byte{"msg1": body}},
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: uint32(len(body))},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	// TOP 1 1 → headers + blank line + first body line only
	send(t, c, "TOP 1 1")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("TOP: expected +OK, got %q", resp)
	}
	lines := readUntilDot(t, r)
	found := false
	for _, l := range lines {
		if strings.Contains(l, "Line1") {
			found = true
		}
		if strings.Contains(l, "Line2") {
			t.Fatalf("TOP 1 should only return 1 body line, got Line2 in %v", lines)
		}
	}
	if !found {
		t.Fatalf("TOP 1: expected Line1 in response, got %v", lines)
	}
}

func TestSession_Quit_AppliesDeletes(t *testing.T) {
	removed := make([]string, 0)
	mbox := &mockMailbox{bodies: map[string][]byte{"msg1": []byte("From: x\r\n\r\nbody\r\n")}}
	// Override Remove to track calls.
	mboxRecorder := &recordingMailbox{mockMailbox: mbox, removedFiles: &removed}

	opts := newTestOpts(
		&mockAuth{users: map[string]string{"u": "p"}},
		mboxRecorder,
		&mockIndex{msgs: []*mailbox.MessageMeta{
			{UID: 1, Filename: "msg1", Size: 10},
		}},
	)
	c, r := newPOP3Session(t, opts)
	login(t, c, r, "u", "p")

	send(t, c, "DELE 1")
	readline(t, r) // +OK

	send(t, c, "QUIT")
	resp := readline(t, r)
	if !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT after DELE: expected +OK, got %q", resp)
	}
	if len(removed) != 1 || removed[0] != "msg1" {
		t.Fatalf("QUIT: expected msg1 to be removed, got %v", removed)
	}
}

// ---- recording mailbox wrapper ----------------------------------------------

type recordingMailbox struct {
	*mockMailbox
	removedFiles *[]string
}

func (r *recordingMailbox) Remove(_, _, filename string) error {
	*r.removedFiles = append(*r.removedFiles, filename)
	return nil
}

// ---- dot-stuffing tests -----------------------------------------------------

func TestWriteMultiLine_DotStuffing(t *testing.T) {
	var buf bytes.Buffer
	writeMultiLine(&buf, []byte("normal\r\n.dotted\r\n"))
	got := buf.String()
	if !strings.Contains(got, "..dotted") {
		t.Fatalf("dot-stuffing failed: %q", got)
	}
	if !strings.HasSuffix(got, ".\r\n") {
		t.Fatalf("missing terminator: %q", got)
	}
}

func TestWriteTopLines_HeaderBody(t *testing.T) {
	data := []byte("H: v\r\n\r\nB1\r\nB2\r\nB3\r\n")
	var buf bytes.Buffer
	writeTopLines(&buf, data, 1) // only first body line
	got := buf.String()
	if !strings.Contains(got, "H: v") {
		t.Fatalf("header missing: %q", got)
	}
	if !strings.Contains(got, "B1") {
		t.Fatalf("first body line missing: %q", got)
	}
	if strings.Contains(got, "B2") {
		t.Fatalf("second body line must not appear for n=1: %q", got)
	}
}
