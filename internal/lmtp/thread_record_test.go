package lmtp

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// deliverThreaded runs one delivery through the real path and returns the guid
// it produced, so a test asserts on what the sidecar records rather than on
// what the recorder was told.
func deliverThreaded(t *testing.T, s *session, box mailbox.UserMailbox, ui mailbox.UserIndex, info *mailbox.UserInfo, raw string) string {
	t.Helper()
	_, _, guid, err := deliverOne(box, ui, "INBOX", bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
	if err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	s.recordThread(info, info.Username, guid, []byte(raw))
	return hex.EncodeToString(guid[:])
}

func threadingSession(t *testing.T) (*session, mailbox.UserMailbox, mailbox.UserIndex, *mailbox.UserInfo) {
	t.Helper()
	root := t.TempDir()
	mb, idx := maildir.New(), fileindex.New()
	info := &mailbox.UserInfo{
		Username: "alice@x", Home: filepath.Join(root, "alice"),
		MailPath: filepath.Join(root, "alice", "Maildir"),
	}
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := idx.OpenUser(info)
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	s := &session{opts: Options{
		Mailbox: mb, Index: idx,
		Threads: threads.NewRecorder(threads.NewCache(time.Minute)),
	}}
	return s, box, ui, info
}

// Delivery is where a conversation is recorded, so the assertion is on the
// sidecar the delivery left behind -- not on the recorder, which has its own
// tests and would pass here even if the hook were never called.
func TestDeliveryRecordsTheConversation(t *testing.T) {
	s, box, ui, info := threadingSession(t)

	first := deliverThreaded(t, s, box, ui, info,
		"Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n")
	second := deliverThreaded(t, s, box, ui, info,
		"Message-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nSubject: Re: Plan\r\n\r\nbody\r\n")

	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatalf("the delivery wrote no sidecar: %v", err)
	}
	a, okA := st.ThreadOfGUID(first)
	b, okB := st.ThreadOfGUID(second)
	if !okA || !okB {
		t.Fatalf("a delivered message is missing from the sidecar: %v / %v", okA, okB)
	}
	if a != b {
		t.Errorf("the reply is in %q and the original in %q", b, a)
	}
	// The conversation is named after the message that started it, which is
	// what makes the sidecar rebuildable by the migration step.
	if a != first {
		t.Errorf("thread = %q, want the first message's guid %q", a, first)
	}
}

// Threading off leaves the account exactly as it is today: no file, no work on
// the delivery path. That is also what every account looks like before the
// migration step reaches it.
func TestDeliveryWritesNothingWhenThreadingIsOff(t *testing.T) {
	s, box, ui, info := threadingSession(t)
	s.opts.Threads = nil

	deliverThreaded(t, s, box, ui, info, "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n")

	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Threads(); len(got) != 0 {
		t.Errorf("threads = %v with threading disabled, want none", got)
	}
}

// A sidecar that cannot be written must not fail the delivery: mail is
// authoritative and this file is derived, so the message lands and the
// conversation is rebuilt later.
func TestADeliverySurvivesAnUnwritableSidecar(t *testing.T) {
	s, box, ui, info := threadingSession(t)
	// A control root that cannot hold a file: the sidecar path is now a
	// directory's child under a path that is itself a file.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	info.ControlDir = filepath.Join(blocked, "sub")

	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	_, _, guid, err := deliverOne(box, ui, "INBOX", bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
	if err != nil {
		t.Fatalf("the delivery itself failed: %v", err)
	}
	// The hook logs and returns; it must not panic and must not report upward.
	s.recordThread(info, info.Username, guid, []byte(raw))
}

// statusSink collects per-recipient results, which is all LMTPData needs from
// its collector.
type statusSink struct{ errs map[string]error }

func (s *statusSink) SetStatus(rcpt string, err error) {
	if s.errs == nil {
		s.errs = map[string]error{}
	}
	s.errs[rcpt] = err
}

// Through LMTPData, not through the hook: the previous rows prove the recorder
// works, and would keep passing if the delivery loop stopped calling it. This
// one fails if the call site goes away -- which is the only thing it is for.
func TestTheDeliveryLoopRecordsTheConversation(t *testing.T) {
	root := t.TempDir()
	mb, idx := maildir.New(), fileindex.New()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}

	s := &session{opts: Options{
		Mailbox:  mb,
		Index:    idx,
		Resolver: resolver,
		Threads:  threads.NewRecorder(threads.NewCache(time.Minute)),
	}}
	s.from = "sender@x"
	s.rcpts = []string{"alice@example.com"}

	sink := &statusSink{}
	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	if err := s.LMTPData(strings.NewReader(raw), sink); err != nil {
		t.Fatalf("LMTPData: %v", err)
	}
	if err := sink.errs["alice@example.com"]; err != nil {
		t.Fatalf("delivery reported: %v", err)
	}

	info := resolver.UserInfo("alice@example.com", "")
	st, err := threads.Load(threads.PathFor(info))
	if err != nil {
		t.Fatalf("load sidecar: %v", err)
	}
	if got := st.Threads(); len(got) != 1 {
		t.Errorf("threads after one delivery = %v, want one -- the delivery loop did not record it", got)
	}
}
