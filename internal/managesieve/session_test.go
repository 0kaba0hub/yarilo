package managesieve

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/sieve"
)

type testClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (c *testClient) close() { c.conn.Close() }

func (c *testClient) send(line string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line)); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// readUntilResult reads lines until it sees OK, NO, or BYE.
func (c *testClient) readUntilResult() (lines []string, ok bool) {
	c.t.Helper()
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, "OK") {
			return lines, true
		}
		if strings.HasPrefix(line, "NO") || strings.HasPrefix(line, "BYE") {
			return lines, false
		}
	}
}

func newTestStore() *sieve.FsScriptStore {
	return &sieve.FsScriptStore{DefaultName: sieve.FallbackDefaultName, Locker: nil}
}

func runSession(t *testing.T, store *sieve.FsScriptStore, homeDir string) *testClient {
	t.Helper()
	client, server := net.Pipe()
	sess := &session{
		conn:     server,
		r:        bufio.NewReader(server),
		w:        bufio.NewWriter(server),
		username: "u1@example.com",
		homeDir:  homeDir,
		store:    store,
		maxSize:  65536,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go sess.serve(ctx)

	tc := &testClient{t: t, conn: client, r: bufio.NewReader(client)}
	t.Cleanup(tc.close)
	_, ok := tc.readUntilResult()
	if !ok {
		t.Fatal("session greeting returned non-OK")
	}
	return tc
}

type sessionCase struct {
	name  string
	setup func(ctx context.Context, store *sieve.FsScriptStore, homeDir string)
	steps []struct {
		send    string
		wantOK  bool
		wantIn  string
		wantNot string
	}
}

var sessionCases = []sessionCase{
	{
		name: "CAPABILITY",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "CAPABILITY\r\n", wantOK: true, wantIn: "IMPLEMENTATION"},
		},
	},
	{
		name: "LISTSCRIPTS empty",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "LISTSCRIPTS\r\n", wantOK: true},
		},
	},
	{
		name: "PUTSCRIPT and LISTSCRIPTS",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "PUTSCRIPT \"test.sieve\" {6+}\r\nkeep;\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantIn: "test.sieve"},
		},
	},
	{
		name: "PUTSCRIPT with invalid script",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "PUTSCRIPT \"bad.sieve\" {15+}\r\nrequire INVALID;\n", wantOK: false},
		},
	},
	{
		name: "GETSCRIPT",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "my.sieve", []byte("keep;\n"))
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "GETSCRIPT \"my.sieve\"\r\n", wantOK: true, wantIn: "keep;"},
		},
	},
	{
		name: "GETSCRIPT nonexistent",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "GETSCRIPT \"missing.sieve\"\r\n", wantOK: false, wantIn: "NONEXISTENT"},
		},
	},
	{
		name: "SETACTIVE and LISTSCRIPTS shows ACTIVE",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "a.sieve", []byte("keep;\n"))
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "SETACTIVE \"a.sieve\"\r\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantIn: "ACTIVE"},
		},
	},
	{
		name: "SETACTIVE empty deactivates",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "a.sieve", []byte("keep;\n"))
			_ = store.SetActive(ctx, "u1@example.com", homeDir, "a.sieve")
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "SETACTIVE \"\"\r\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantNot: "ACTIVE"},
		},
	},
	{
		name: "DELETESCRIPT",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "del.sieve", []byte("keep;\n"))
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "DELETESCRIPT \"del.sieve\"\r\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantNot: "del.sieve"},
		},
	},
	{
		name: "DELETESCRIPT active returns error",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "active.sieve", []byte("keep;\n"))
			_ = store.SetActive(ctx, "u1@example.com", homeDir, "active.sieve")
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "DELETESCRIPT \"active.sieve\"\r\n", wantOK: false, wantIn: "ACTIVE"},
		},
	},
	{
		name: "CHECKSCRIPT valid",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "CHECKSCRIPT {6+}\r\nkeep;\n", wantOK: true},
		},
	},
	{
		name: "CHECKSCRIPT invalid",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "CHECKSCRIPT {5+}\r\nBAD!!", wantOK: false},
		},
	},
	{
		name: "RENAMESCRIPT",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "old.sieve", []byte("keep;\n"))
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "RENAMESCRIPT \"old.sieve\" \"new.sieve\"\r\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantIn: "new.sieve", wantNot: "old.sieve"},
		},
	},
	{
		name: "RENAMESCRIPT active follows",
		setup: func(ctx context.Context, store *sieve.FsScriptStore, homeDir string) {
			_ = store.SaveScript(ctx, "u1@example.com", homeDir, "old.sieve", []byte("keep;\n"))
			_ = store.SetActive(ctx, "u1@example.com", homeDir, "old.sieve")
		},
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "RENAMESCRIPT \"old.sieve\" \"new.sieve\"\r\n", wantOK: true},
			{send: "LISTSCRIPTS\r\n", wantOK: true, wantIn: "ACTIVE"},
		},
	},
	{
		name: "NOOP",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "NOOP\r\n", wantOK: true},
		},
	},
	{
		name: "NOOP with tag",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "NOOP \"mytag\"\r\n", wantOK: true, wantIn: "mytag"},
		},
	},
	{
		name: "unknown command",
		steps: []struct {
			send    string
			wantOK  bool
			wantIn  string
			wantNot string
		}{
			{send: "BOGUSCMD\r\n", wantOK: false},
		},
	},
}

func TestSession(t *testing.T) {
	for _, tc := range sessionCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore()
			homeDir := t.TempDir()
			if tc.setup != nil {
				tc.setup(context.Background(), store, homeDir)
			}
			c := runSession(t, store, homeDir)
			for _, step := range tc.steps {
				c.send(step.send)
				lines, ok := c.readUntilResult()
				joined := strings.Join(lines, "\n")
				if ok != step.wantOK {
					t.Errorf("send %q: got ok=%v, want %v\nresponse: %s",
						step.send, ok, step.wantOK, joined)
				}
				if step.wantIn != "" && !strings.Contains(joined, step.wantIn) {
					t.Errorf("send %q: expected %q in response\nresponse: %s",
						step.send, step.wantIn, joined)
				}
				if step.wantNot != "" && strings.Contains(joined, step.wantNot) {
					t.Errorf("send %q: unexpected %q in response\nresponse: %s",
						step.send, step.wantNot, joined)
				}
			}
		})
	}
}
