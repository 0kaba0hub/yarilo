package managesieve

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/sieve"
	"github.com/0kaba0hub/yarilo/pkg/dict"
	_ "github.com/0kaba0hub/yarilo/pkg/dict/memory"
)

func newMemDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// testClient wraps the client side of a net.Pipe() and reads ManageSieve responses.
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

// readUntilResult reads lines until it sees OK, NO, or BYE. Returns the full
// response lines and whether the final status line starts with "OK".
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

// runSession starts a session goroutine and returns a testClient connected to it.
// The greeting is consumed automatically; callers start from the first command.
func runSession(t *testing.T, d dict.Dict) *testClient {
	t.Helper()
	client, server := net.Pipe()
	sess := &session{
		conn:     server,
		r:        bufio.NewReader(server),
		w:        bufio.NewWriter(server),
		username: "u1@example.com",
		homeDir:  "/home/u1",
		dict:     d,
		maxSize:  65536,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go sess.serve(ctx)

	tc := &testClient{t: t, conn: client, r: bufio.NewReader(client)}
	t.Cleanup(tc.close)
	// Consume the greeting.
	_, ok := tc.readUntilResult()
	if !ok {
		t.Fatal("session greeting returned non-OK")
	}
	return tc
}

type sessionCase struct {
	name  string
	setup func(ctx context.Context, d dict.Dict)
	steps []struct {
		send    string
		wantOK  bool
		wantIn  string // substring expected anywhere in response
		wantNot string // substring expected absent from response
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "my.sieve", []byte("keep;\n"))
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "a.sieve", []byte("keep;\n"))
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "a.sieve", []byte("keep;\n"))
			_ = sieve.SetActive(context.Background(), d, "u1@example.com", "/home/u1", "a.sieve")
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "del.sieve", []byte("keep;\n"))
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "active.sieve", []byte("keep;\n"))
			_ = sieve.SetActive(context.Background(), d, "u1@example.com", "/home/u1", "active.sieve")
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "old.sieve", []byte("keep;\n"))
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
		setup: func(ctx context.Context, d dict.Dict) {
			_ = sieve.SaveScript(ctx, d, "u1@example.com", "/home/u1", "old.sieve", []byte("keep;\n"))
			_ = sieve.SetActive(context.Background(), d, "u1@example.com", "/home/u1", "old.sieve")
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
			d := newMemDict(t)
			if tc.setup != nil {
				tc.setup(context.Background(), d)
			}
			c := runSession(t, d)
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
