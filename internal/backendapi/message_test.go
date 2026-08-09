package backendapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const damagedMsg = "From: alice@example.com\r\n" +
	"Subject: damaged\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"body wordzz here\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcTl8uXrp/Og0MTGCg==\r\n" +
	"--BOUND--\r\n"

// messageServer stores one message and returns the server plus the stored
// message's identity, so a test can ask for it either way.
func messageServer(t *testing.T) (*httptest.Server, string, uint32, string) {
	t.Helper()
	return messageServerRaw(t, damagedMsg)
}

// messageServerWith stores one message of the caller's choosing.
func messageServerWith(t *testing.T, raw string) (*httptest.Server, string, uint32) {
	t.Helper()
	ts, user, uid, _ := messageServerRaw(t, raw)
	return ts, user, uid
}

func messageServerRaw(t *testing.T, msg string) (*httptest.Server, string, uint32, string) {
	t.Helper()
	root := t.TempDir()
	mb := maildir.New()
	idx := file.New()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}

	const user = "alice@example.com"
	info := resolver.UserInfo(user, "")
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := idx.OpenUser(info)
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(msg), 1, int64(len(msg)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: uint32(len(msg)), VSize: vsize, GUID: guid,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = ui.Close()
	_ = box.Close()

	s := New(Options{
		Mailbox:  mb,
		Index:    file.New(),
		Resolver: resolver,
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, user, 1, mailbox.FormatObjectID(guid)
}

func getMessage(t *testing.T, ts *httptest.Server, body map[string]any) (int, string) {
	t.Helper()
	code, out := doJSON(t, ts, http.MethodPost, "/api/backend/message/get", "", body)
	return code, string(out)
}

// raw is the message as it is on disk: this is the tool reached for when the
// bytes themselves are the problem, so anything but byte-for-byte is the wrong
// answer.
func TestMessageGetRawIsTheMessageItself(t *testing.T) {
	ts, user, uid, _ := messageServer(t)
	code, got := getMessage(t, ts, map[string]any{
		"user": user, "folder": "INBOX", "uid": uid, "mode": "raw",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, got)
	}
	if got != damagedMsg {
		t.Errorf("raw output differs from the stored message:\n%q", got)
	}
}

// mime keeps the structure and leaves the payload out: the headers of every
// part are what says where a message is malformed, and the base64 behind them
// is what makes a mailbox unreadable in a terminal.
func TestMessageGetMIMEKeepsStructureAndDropsBodies(t *testing.T) {
	ts, user, uid, _ := messageServer(t)
	code, got := getMessage(t, ts, map[string]any{
		"user": user, "folder": "INBOX", "uid": uid, "mode": "mime",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, got)
	}
	for _, want := range []string{"Subject: damaged", "multipart/mixed", "application/pdf", "bytes of body elided"} {
		if !strings.Contains(got, want) {
			t.Errorf("mime output does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "JVBERi0xLjQK") {
		t.Errorf("mime output carries the attachment payload:\n%s", got)
	}
	if strings.Contains(got, "body wordzz here") {
		t.Errorf("mime output carries a part body:\n%s", got)
	}
}

// The GUID from a log line is enough to fetch the message: that is the whole
// reason the log carries one.
func TestMessageGetByGUID(t *testing.T) {
	ts, user, _, guid := messageServer(t)
	code, got := getMessage(t, ts, map[string]any{
		"user": user, "folder": "INBOX", "guid": guid, "mode": "raw",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, got)
	}
	if got != damagedMsg {
		t.Errorf("the GUID resolved to something else:\n%q", got)
	}
}

// Two ways to name one message is a caller that does not know which it means,
// and picking one silently is how the wrong mail gets read.
func TestMessageGetRefusesAmbiguousNaming(t *testing.T) {
	ts, user, uid, guid := messageServer(t)
	for _, body := range []map[string]any{
		{"user": user, "folder": "INBOX", "uid": uid, "guid": guid, "mode": "raw"},
		{"user": user, "folder": "INBOX", "mode": "raw"},
	} {
		if code, _ := getMessage(t, ts, body); code != http.StatusBadRequest {
			t.Errorf("status = %d for %v, want 400", code, body)
		}
	}
}

// Reading a message must not change it. A diagnostic that marks mail as seen,
// moves a modseq or advances an index answers a different question than the
// one that was asked -- and the next reader sees a mailbox this call touched.
func TestMessageGetChangesNothing(t *testing.T) {
	ts, user, uid, _ := messageServer(t)
	before := folderStateOf(t, ts, user)

	for _, mode := range []string{"raw", "mime"} {
		if code, out := getMessage(t, ts, map[string]any{
			"user": user, "folder": "INBOX", "uid": uid, "mode": mode,
		}); code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", mode, code, out)
		}
	}

	if after := folderStateOf(t, ts, user); after != before {
		t.Errorf("folder state changed by reading a message:\nbefore %s\nafter  %s", before, after)
	}
}

// folderStateOf is the folder's own account of itself: counters and sequence
// numbers, which is what a read would disturb if it disturbed anything.
func folderStateOf(t *testing.T, ts *httptest.Server, user string) string {
	t.Helper()
	_, out := doJSON(t, ts, http.MethodPost, "/api/backend/folder/info", "",
		map[string]any{"user": user, "folder": "INBOX"})
	return string(out)
}

// unparseableMsg is the shape the parser refuses: the first line cannot be a
// header. The stored fixture above is only damaged in meaning -- it parses --
// so it never reaches the fallback this test is about.
const unparseableMsg = "NoColonHeaderLine\r\nFrom: alice@example.com\r\n\r\nbody wordzz\r\n"

// The command exists for messages whose structure will not parse, so that path
// must hand over the message -- from its first byte. The parser consumes part
// of the stream before it gives up, and streaming what is left would show an
// operator a message that starts mid-header: damage that is not there.
func TestMessageGetMIMEFallsBackToTheWholeMessage(t *testing.T) {
	ts, user, uid := messageServerWith(t, unparseableMsg)

	code, got := getMessage(t, ts, map[string]any{
		"user": user, "folder": "INBOX", "uid": uid, "mode": "mime",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, got)
	}
	if !strings.Contains(got, "MIME structure unreadable") {
		t.Errorf("the output does not say why it is not an outline:\n%s", got)
	}
	if !strings.Contains(got, "NoColonHeaderLine") {
		t.Errorf("the output starts past the beginning of the message:\n%s", got)
	}
	if !strings.Contains(got, "body wordzz") {
		t.Errorf("the output does not carry the whole message:\n%s", got)
	}
}

// The audit line's byte count is the point of the audit line: it says how much
// of a mailbox left the server. A count that is not the number of bytes
// written is worse than none, because it reads as precise.
func TestMessageGetAuditCountMatchesWhatWasSent(t *testing.T) {
	for _, tc := range []struct{ name, msg, mode string }{
		{"raw", damagedMsg, "raw"},
		{"outline", damagedMsg, "mime"},
		// The fallback writes a marker and then the message: two writes, one
		// count, and the easiest place to add them up twice.
		{"unreadable structure", unparseableMsg, "mime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, user, uid := messageServerWith(t, tc.msg)

			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			code, got := getMessage(t, ts, map[string]any{
				"user": user, "folder": "INBOX", "uid": uid, "mode": tc.mode,
			})
			if code != http.StatusOK {
				t.Fatalf("status = %d: %s", code, got)
			}
			reported := auditBytes(t, logs.String())
			if reported != len(got) {
				t.Errorf("audit says %d bytes, %d were sent", reported, len(got))
			}
		})
	}
}

// auditBytes reads the count out of the access line.
func auditBytes(t *testing.T, logs string) int {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "message content read") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if rest, ok := strings.CutPrefix(field, "bytes="); ok {
				n, err := strconv.Atoi(rest)
				if err != nil {
					t.Fatalf("bytes=%q is not a number", rest)
				}
				return n
			}
		}
	}
	t.Fatalf("no access line in the log:\n%s", logs)
	return 0
}
