package imap_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadIDServer seeds an account with a conversation -- recorded exactly as a
// delivery records it -- and serves it over IMAP.
//
// Returns the client and the two message ids as OBJECTID spells them, because
// the whole answer hangs on IMAP and the sidecar spelling a message id the
// same way. A drift there silently turns every THREADID into NIL, which is a
// legal answer for an unmigrated account -- so the defect would be
// indistinguishable from the ordinary state.
func threadIDServer(t *testing.T, withSidecar bool) (net.Conn, *bufio.Reader, string, string) {
	conn, rd, _, first, second := threadIDServerIdle(t, withSidecar, time.Minute)
	return conn, rd, first, second
}

// threadIDServerIdle is the same, with the fold cache's idleness exposed. A
// NEGATIVE idle disables caching, which turns Cache.Folds() into a count of
// Get calls -- the only honest way to observe, from outside, how often a
// command reads the sidecar.
func threadIDServerIdle(t *testing.T, withSidecar bool, idle time.Duration) (net.Conn, *bufio.Reader, *threads.Cache, string, string) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	info := resolver.UserInfo("user@test.com", "")

	mb, idx := maildir.New(), fileindex.New()
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := idx.OpenUser(info)

	cache := threads.NewCache(idle)
	rec := threads.NewRecorder(cache)
	path := threads.PathFor(info)

	raws := []string{
		"Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n",
		"Message-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nSubject: Re: Plan\r\n\r\nbody\r\n",
	}
	var ids []string
	for i, raw := range raws {
		uid := uint32(i + 1)
		name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), uid, int64(len(raw)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		f, err := ui.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatalf("open folder: %v", err)
		}
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(raw)), VSize: vsize,
			GUID: guid, InternalDate: time.Now(),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if withSidecar {
			if _, err := rec.Record(info.Username, path, mailbox.FormatObjectID(guid), []byte(raw)); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		ids = append(ids, mailbox.FormatObjectID(guid))
	}
	ui.Close()  //nolint:errcheck
	box.Close() //nolint:errcheck

	opts := imapserver.Options{
		Mailbox:  maildir.New(),
		Index:    fileindex.New(),
		Resolver: resolver,
		Auth:     &stubPassdb{user: "user@test.com", pass: "testpass"},
	}
	if withSidecar {
		opts.Threads = cache
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	// Raw wire, not the library client: its FetchMessageBuffer does not carry
	// the OBJECTID fields, and the claim here is precisely about the bytes we
	// emit for THREADID.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rd := bufio.NewReader(conn)
	readUntilTag(t, rd, "") // greeting
	command(t, conn, rd, "a1", "LOGIN user@test.com testpass")
	command(t, conn, rd, "a2", "SELECT INBOX")
	return conn, rd, cache, ids[0], ids[1]
}

// command sends one tagged command and returns every untagged line before the
// tagged reply, which is where FETCH data lives.
func command(t *testing.T, conn net.Conn, rd *bufio.Reader, tag, cmd string) []string {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, cmd); err != nil {
		t.Fatalf("write %s: %v", cmd, err)
	}
	return readUntilTag(t, rd, tag)
}

func readUntilTag(t *testing.T, rd *bufio.Reader, tag string) []string {
	t.Helper()
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if tag == "" {
			return []string{line} // the greeting is one line
		}
		if strings.HasPrefix(line, tag+" ") {
			if !strings.HasPrefix(line, tag+" OK") {
				t.Fatalf("command failed: %s", line)
			}
			return lines
		}
		lines = append(lines, line)
	}
}

// fetchIDs asks for both object ids and returns, per sequence number, the
// EMAILID and THREADID exactly as they appeared on the wire ("" for NIL).
func fetchIDs(t *testing.T, conn net.Conn, rd *bufio.Reader) map[int][2]string {
	t.Helper()
	lines := command(t, conn, rd, "a3", "FETCH 1:2 (EMAILID THREADID)")
	out := map[int][2]string{}
	for _, line := range lines {
		var seq int
		if _, err := fmt.Sscanf(line, "* %d FETCH", &seq); err != nil {
			continue
		}
		out[seq] = [2]string{fieldAfter(line, "EMAILID"), fieldAfter(line, "THREADID")}
	}
	if len(out) != 2 {
		t.Fatalf("FETCH returned %d messages: %v", len(out), lines)
	}
	return out
}

// fieldAfter reads the token following name, mapping NIL to the empty string.
func fieldAfter(line, name string) string {
	at := strings.Index(line, name+" ")
	if at < 0 {
		return ""
	}
	rest := strings.TrimLeft(line[at+len(name)+1:], " (")
	end := strings.IndexAny(rest, " )")
	if end >= 0 {
		rest = rest[:end]
	}
	if rest == "NIL" {
		return ""
	}
	return rest
}

// OBJECTID is advertised, so THREADID is a promise -- and it answered NIL for
// every message. It now answers the conversation the delivery recorded, from
// the same sidecar JMAP reads: a second threading implementation on this side
// would be two answers to one question about one mailbox.
func TestFetchThreadIDNamesTheConversation(t *testing.T) {
	conn, rd, root, reply := threadIDServer(t, true)
	got := fetchIDs(t, conn, rd)

	if got[1][0] != root || got[2][0] != reply {
		t.Fatalf("EMAILID = %q/%q, want %q/%q -- the ids do not line up", got[1][0], got[2][0], root, reply)
	}
	if got[1][1] != root {
		t.Errorf("THREADID of the first message = %q, want %q", got[1][1], root)
	}
	if got[2][1] != root {
		t.Errorf("THREADID of the reply = %q, want the conversation %q -- it reported %v",
			got[2][1], root, got[2][1] == reply)
	}
}

// Without a sidecar the answer is NIL, which RFC 8474 defines as "the server
// cannot determine a thread". That is exactly true of an account the migration
// step has not reached, and is what this returned for every message before
// threading existed -- so the fallback is stated here rather than being what
// every other row silently proves.
func TestFetchThreadIDIsNilWithoutASidecar(t *testing.T) {
	conn, rd, root, reply := threadIDServer(t, false)
	got := fetchIDs(t, conn, rd)

	if got[1][0] != root || got[2][0] != reply {
		t.Fatalf("EMAILID = %q/%q, want %q/%q", got[1][0], got[2][0], root, reply)
	}
	if got[1][1] != "" || got[2][1] != "" {
		t.Errorf("THREADID = %q/%q with no sidecar, want NIL for both", got[1][1], got[2][1])
	}
}

// The sidecar is read once per FETCH, not once per message.
//
// Two defects share this one cause, and the fix is the same movement for both.
// A FETCH 1:* (THREADID) over a large mailbox stats and folds the sidecar once
// per message -- tens of thousands of times for one command. And each message
// would answer out of its own instant, so a merge arriving mid-command yields
// a set of answers stitched from two states: some messages naming a
// conversation that the rest of the same response says no longer exists.
//
// Caching is disabled here on purpose, which makes Folds() a count of reads:
// with a cache, the second read is free and the row cannot tell the two
// implementations apart. What is pinned is the ratio -- one read per command,
// whatever the size of the fetch -- so the per-message version fails this row
// with a number equal to the message count.
func TestFetchReadsTheSidecarOncePerCommand(t *testing.T) {
	conn, rd, cache, root, _ := threadIDServerIdle(t, true, -1)
	before := cache.Folds()

	got := fetchIDs(t, conn, rd)
	if got[2][1] != root {
		t.Fatalf("THREADID of the reply = %q, want %q -- the answer must still be right", got[2][1], root)
	}

	if folds := cache.Folds() - before; folds != 1 {
		t.Errorf("FETCH of 2 messages read the sidecar %d times, want 1 -- it is resolved per message, so a large mailbox pays per message and answers from as many states", folds)
	}
}

// A FETCH that asks for no conversations reads no sidecar. Resolving per
// command is only cheaper if the command that does not want it pays nothing.
func TestFetchWithoutThreadIDReadsNoSidecar(t *testing.T) {
	conn, rd, cache, _, _ := threadIDServerIdle(t, true, -1)
	before := cache.Folds()

	command(t, conn, rd, "a3", "FETCH 1:2 (UID FLAGS)")

	if folds := cache.Folds() - before; folds != 0 {
		t.Errorf("a FETCH that never mentions THREADID read the sidecar %d times, want 0", folds)
	}
}
