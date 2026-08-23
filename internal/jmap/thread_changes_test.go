package jmap

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadedServer seeds an account and hands back the pieces a test needs to
// deliver more mail into it afterwards, because what these rows are about is
// what a client learns BETWEEN two states.
type threadedServer struct {
	srv  *Server
	info *mailbox.UserInfo
	box  mailbox.UserMailbox
	idx  mailbox.UserIndex
	rec  *threads.Recorder
	path string
	uid  uint32
	ids  []string
}

func newThreadedServer(t *testing.T) *threadedServer {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	idx := fileindex.New(fileindex.WithLocker(locker)).OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck

	cache := threads.NewCache(time.Minute)
	ts := &threadedServer{
		info: info, box: box, idx: idx,
		rec:  threads.NewRecorder(cache),
		path: threads.PathFor(info),
	}
	ts.srv = New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       fileindex.New(fileindex.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
			Threads:     cache,
		},
	})
	return ts
}

// deliver adds one message and records its conversation, exactly as LMTP does.
func (ts *threadedServer) deliver(t *testing.T, raw string) string {
	t.Helper()
	ts.uid++
	name, vsize, guid, err := ts.box.Save("INBOX", strings.NewReader(raw), ts.uid, int64(len(raw)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ts.idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := ts.idx.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: ts.uid, Filename: name, Size: uint32(len(raw)), VSize: vsize,
		GUID: guid, InternalDate: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	id := mailbox.FormatObjectID(guid)
	if _, err := ts.rec.Record(testUser, ts.path, id, []byte(raw)); err != nil {
		t.Fatalf("record: %v", err)
	}
	ts.ids = append(ts.ids, id)
	return id
}

func (ts *threadedServer) call(t *testing.T, method, args string) map[string]any {
	t.Helper()
	return callAPI(t, ts.srv, fmt.Sprintf(
		`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		  "methodCalls":[[%q,%s,"c0"]]}`, method, args))
}

func idList(t *testing.T, payload map[string]any, field string) []string {
	t.Helper()
	raw, err := json.Marshal(payload[field])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v -- %v", field, err, payload[field])
	}
	return out
}

func threadStateOf(t *testing.T, ts *threadedServer) string {
	t.Helper()
	p := ts.call(t, "Thread/get", fmt.Sprintf(`{"accountId":%q,"ids":[]}`, testUser))
	state, _ := p["state"].(string)
	if state == "" {
		t.Fatalf("Thread/get returned no state: %v", p)
	}
	return state
}

// The review focus of this stage: a merge has to reach the client. The
// swallowed conversation is destroyed -- its id names nothing now -- and the
// surviving one is updated. Silence would leave the old conversation in a
// client's list for ever.
func TestThreadChangesReportsAMergeAsDestroyedAndUpdated(t *testing.T) {
	ts := newThreadedServer(t)
	first := ts.deliver(t, "Message-ID: <a@x>\r\nSubject: One\r\n\r\nbody\r\n")
	second := ts.deliver(t, "Message-ID: <b@x>\r\nSubject: Two\r\n\r\nbody\r\n")
	since := threadStateOf(t, ts)

	// The late message that answers both, joining the two conversations.
	ts.deliver(t, "Message-ID: <c@x>\r\nReferences: <a@x> <b@x>\r\nSubject: Re: One\r\n\r\nbody\r\n")

	payload := ts.call(t, "Thread/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType, _ := payload["type"].(string); errType != "" {
		t.Fatalf("Thread/changes refused: %v", payload)
	}
	destroyed := idList(t, payload, "destroyed")
	updated := idList(t, payload, "updated")

	if len(destroyed) != 1 || destroyed[0] != second {
		t.Errorf("destroyed = %v, want the swallowed conversation %q", destroyed, second)
	}
	var survived bool
	for _, id := range updated {
		survived = survived || id == first
	}
	if !survived {
		t.Errorf("updated = %v, want the surviving conversation %q", updated, first)
	}
	if payload["newState"] == since {
		t.Error("the state did not move after a merge")
	}
}

// A new conversation is created, not updated: a client that sees "updated" for
// an id it has never heard of has nothing to update.
func TestThreadChangesReportsANewConversationAsCreated(t *testing.T) {
	ts := newThreadedServer(t)
	ts.deliver(t, "Message-ID: <a@x>\r\nSubject: One\r\n\r\nbody\r\n")
	since := threadStateOf(t, ts)

	fresh := ts.deliver(t, "Message-ID: <z@x>\r\nSubject: Unrelated\r\n\r\nbody\r\n")

	payload := ts.call(t, "Thread/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	created := idList(t, payload, "created")
	if len(created) != 1 || created[0] != fresh {
		t.Errorf("created = %v, want the new conversation %q", created, fresh)
	}
	if got := idList(t, payload, "destroyed"); len(got) != 0 {
		t.Errorf("destroyed = %v, want none", got)
	}
}

// The half the specification requires and Thread/changes alone cannot give: a
// client syncing Email objects groups by the threadId it holds, so the
// messages of a swallowed conversation must come back as updated -- otherwise
// they keep an id that names nothing and the old conversation never leaves the
// client's list (RFC 8620 §5.2).
func TestEmailChangesReportsMessagesWhoseThreadWasRenamed(t *testing.T) {
	ts := newThreadedServer(t)
	ts.deliver(t, "Message-ID: <a@x>\r\nSubject: One\r\n\r\nbody\r\n")
	moved := ts.deliver(t, "Message-ID: <b@x>\r\nSubject: Two\r\n\r\nbody\r\n")
	since := emailStateOf(t, ts.srv)

	ts.deliver(t, "Message-ID: <c@x>\r\nReferences: <a@x> <b@x>\r\nSubject: Re: One\r\n\r\nbody\r\n")

	payload := ts.call(t, "Email/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType, _ := payload["type"].(string); errType != "" {
		t.Fatalf("Email/changes refused: %v", payload)
	}
	updated := idList(t, payload, "updated")
	var found bool
	for _, id := range updated {
		found = found || id == moved
	}
	if !found {
		t.Errorf("updated = %v, want the message whose threadId changed (%q)", updated, moved)
	}
}

// A state this server did not issue -- another version, another object type,
// or a client's invention -- is refused rather than diffed.
func TestThreadChangesRefusesAStateItDidNotIssue(t *testing.T) {
	ts := newThreadedServer(t)
	ts.deliver(t, "Message-ID: <a@x>\r\nSubject: One\r\n\r\nbody\r\n")

	for _, since := range []string{"0", "hello", emailStateOf(t, ts.srv)} {
		// changesCall, not the plain call: this row is about the refusal, and
		// the helper that fatals on a method error cannot see one.
		payload, errType := changesCall(t, ts.srv, "Thread/changes",
			fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
		if got := errType; got != "cannotCalculateChanges" {
			t.Errorf("sinceState %q answered %q/%v, want cannotCalculateChanges", since, errType, payload)
		}
	}
}
