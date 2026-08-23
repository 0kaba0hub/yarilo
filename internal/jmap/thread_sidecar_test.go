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

// threadedAccount seeds two messages -- a root and its reply -- and the
// sidecar a delivery would have written for them, then builds a server that
// reads it.
//
// It returns the two ids as JMAP spells them, because the whole feature hangs
// on the sidecar and JMAP spelling a message id the same way. Nothing else in
// the tree asserts that: a mismatch sends every lookup down the "no sidecar"
// fallback, which is a legal answer for an unmigrated account -- so the defect
// would be indistinguishable from the ordinary state, and green.
func threadedAccount(t *testing.T) (*Server, string, string) {
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

	raws := []string{
		"Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n",
		"Message-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nSubject: Re: Plan\r\n\r\nbody\r\n",
	}
	cache := threads.NewCache(time.Minute)
	rec := threads.NewRecorder(cache)
	path := threads.PathFor(info)

	var ids []string
	for i, raw := range raws {
		uid := uint32(i + 1)
		name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), uid, int64(len(raw)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		f, err := idx.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatalf("open folder: %v", err)
		}
		if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(raw)), VSize: vsize,
			GUID: guid, InternalDate: time.Now(),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		// Recorded exactly as a delivery records it, id spelling included.
		if _, err := rec.Record(testUser, path, mailbox.FormatObjectID(guid), []byte(raw)); err != nil {
			t.Fatalf("record thread: %v", err)
		}
		ids = append(ids, mailbox.FormatObjectID(guid))
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	s := New(Options{
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
	return s, ids[0], ids[1]
}

// Thread/get answers with the conversation, not with the message it was asked
// about. The reply is in the root's thread, so both ids come back.
func TestThreadGetReturnsTheConversation(t *testing.T) {
	s, root, reply := threadedAccount(t)

	payload := callAPI(t, s, fmt.Sprintf(
		`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		  "methodCalls":[["Thread/get",{"accountId":%q,"ids":[%q]},"c0"]]}`, testUser, root))

	list, _ := payload["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want one thread", payload["list"])
	}
	thread, _ := list[0].(map[string]any)
	raw, _ := json.Marshal(thread["emailIds"])
	var emailIDs []string
	if err := json.Unmarshal(raw, &emailIDs); err != nil {
		t.Fatalf("emailIds: %v -- %v", err, thread)
	}
	if len(emailIDs) != 2 {
		t.Fatalf("emailIds = %v, want both messages -- the sidecar was not read", emailIDs)
	}
	var haveRoot, haveReply bool
	for _, id := range emailIDs {
		haveRoot = haveRoot || id == root
		haveReply = haveReply || id == reply
	}
	if !haveRoot || !haveReply {
		t.Errorf("emailIds = %v, want %q and %q", emailIDs, root, reply)
	}
}

// Email.threadId names the conversation, which for a reply is NOT its own id.
//
// This is the assertion the fallback would otherwise hide: an id-spelling drift
// between JMAP and the sidecar sends every lookup to "the message is its own
// thread", which is exactly what an unmigrated account legitimately answers.
func TestEmailThreadIdNamesTheConversation(t *testing.T) {
	s, root, reply := threadedAccount(t)

	payload := callAPI(t, s, fmt.Sprintf(
		`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		  "methodCalls":[["Email/get",{"accountId":%q,"ids":[%q],"properties":["id","threadId"]},"c0"]]}`,
		testUser, reply))

	list, _ := payload["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want the reply", payload["list"])
	}
	email, _ := list[0].(map[string]any)
	got, _ := email["threadId"].(string)
	if got == reply {
		t.Fatalf("threadId = the reply's own id: the sidecar was not consulted")
	}
	if got != root {
		t.Errorf("threadId = %q, want the conversation %q", got, root)
	}
}

// And with no sidecar the account answers as it did before threading existed:
// every message its own conversation. The row is here so the fallback is a
// stated behaviour rather than the thing every other row silently proves.
func TestWithoutASidecarEveryMessageIsItsOwnThread(t *testing.T) {
	s, id, _ := storedServerWithMessageAt(t, setTestMessage, 0)

	payload := callAPI(t, s, fmt.Sprintf(
		`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		  "methodCalls":[["Email/get",{"accountId":%q,"ids":[%q],"properties":["id","threadId"]},"c0"]]}`,
		testUser, id))

	list, _ := payload["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", payload["list"])
	}
	email, _ := list[0].(map[string]any)
	if got, _ := email["threadId"].(string); got != id {
		t.Errorf("threadId = %q, want the message's own id %q", got, id)
	}
}
