package jmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const setTestMessage = "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nbody\r\n"

// storedFlags reads the message back through the STORE, not through JMAP. A
// test that asserted only the Email/set response would pass on a write that
// never reached the index -- which is the failure this method is most likely to
// have, and the one the issue names.
func storedFlags(t *testing.T, home string) (flags, keywords []string) {
	t.Helper()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	ui := file.New().OpenUser(info)
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("store holds %d messages, want 1", len(msgs))
	}
	flags = append([]string(nil), msgs[0].Flags...)
	keywords = append([]string(nil), msgs[0].Keywords...)
	sort.Strings(flags)
	sort.Strings(keywords)
	return flags, keywords
}

// emailSetCall posts one Email/set and returns its decoded response.
func emailSetCall(t *testing.T, s *Server, args string) jmapcore.SetResponse {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["Email/set",%s,"c0"]]}`, args)
	w := postAPIRaw(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.MethodResponses) != 1 {
		t.Fatalf("got %d responses: %s", len(resp.MethodResponses), w.Body)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("method name: %v", err)
	}
	if name != "Email/set" {
		t.Fatalf("method responded as %q: %s", name, w.Body)
	}
	var out jmapcore.SetResponse
	if err := json.Unmarshal(resp.MethodResponses[0][1], &out); err != nil {
		t.Fatalf("decode Email/set response: %v -- %s", err, w.Body)
	}
	return out
}

// The mutation has to land in the store, as the corresponding IMAP change: a
// JMAP keyword is an IMAP flag or an IMAP keyword, and asserting the JMAP round
// trip alone would pass on a write that stopped in memory.
func TestEmailSetKeywordsReachTheStore(t *testing.T) {
	tests := []struct {
		name         string
		patch        string
		wantFlags    []string
		wantKeywords []string
	}{
		{
			// The message is delivered \Seen, so this is the operation a client
			// performs constantly: marking it unread again.
			name:         "removing a system keyword clears the flag",
			patch:        `{"keywords":{}}`,
			wantFlags:    nil,
			wantKeywords: nil,
		},
		{
			name:         "a patch entry removes one keyword",
			patch:        `{"keywords/$seen":null}`,
			wantFlags:    nil,
			wantKeywords: nil,
		},
		{
			name:         "false removes as null does",
			patch:        `{"keywords/$seen":false}`,
			wantFlags:    nil,
			wantKeywords: nil,
		},
		{
			name:         "a patch entry adds a system keyword",
			patch:        `{"keywords/$flagged":true}`,
			wantFlags:    []string{`\Flagged`, `\Seen`},
			wantKeywords: nil,
		},
		{
			// The half that is not a system flag: a custom keyword has to reach
			// the store as a keyword, which is the write #1281 made cheap.
			name:         "a custom keyword is stored as a keyword",
			patch:        `{"keywords/$work":true}`,
			wantFlags:    []string{`\Seen`},
			wantKeywords: []string{"$work"},
		},
		{
			name:         "a whole keywords object replaces the set",
			patch:        `{"keywords":{"$answered":true,"$label1":true}}`,
			wantFlags:    []string{`\Answered`},
			wantKeywords: []string{"$label1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)
			resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:%s}}`, testUser, id, tc.patch))

			if len(resp.NotUpdated) != 0 {
				t.Fatalf("update refused: %+v", resp.NotUpdated[id])
			}
			if _, ok := resp.Updated[id]; !ok {
				t.Fatalf("id %q is in neither updated nor notUpdated -- the client cannot tell what happened", id)
			}

			flags, keywords := storedFlags(t, home)
			if !equalStrings(flags, tc.wantFlags) {
				t.Errorf("stored flags = %v, want %v", flags, tc.wantFlags)
			}
			if !equalStrings(keywords, tc.wantKeywords) {
				t.Errorf("stored keywords = %v, want %v", keywords, tc.wantKeywords)
			}
		})
	}
}

// What is not built has to be visible in the response. A client that asks for a
// move and is told "updated" believes the message moved; the message staying
// put is asserted here as well, so the row fails on a silent accept rather than
// only on a wrong error string.
func TestEmailSetRefusesWhatIsNotImplemented(t *testing.T) {
	tests := []struct {
		name     string
		args     func(id string) string
		bucket   func(jmapcore.SetResponse, string) *jmapcore.SetError
		wantType string
	}{
		{
			name:     "create",
			args:     func(string) string { return fmt.Sprintf(`{"accountId":%q,"create":{"k1":{}}}`, testUser) },
			bucket:   func(r jmapcore.SetResponse, string2 string) *jmapcore.SetError { return r.NotCreated["k1"] },
			wantType: jmapcore.SetErrNotImplemented,
		},
		{
			name: "destroy",
			args: func(id string) string {
				return fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testUser, id)
			},
			bucket:   func(r jmapcore.SetResponse, id string) *jmapcore.SetError { return r.NotDestroyed[id] },
			wantType: jmapcore.SetErrNotImplemented,
		},
		{
			name: "a move, which would look like success if ignored",
			args: func(id string) string {
				return fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds":{"other":true}}}}`, testUser, id)
			},
			bucket:   func(r jmapcore.SetResponse, id string) *jmapcore.SetError { return r.NotUpdated[id] },
			wantType: jmapcore.SetErrNotImplemented,
		},
		{
			name: "an unknown id",
			args: func(string) string {
				return fmt.Sprintf(`{"accountId":%q,"update":{"nosuchid":{"keywords":{}}}}`, testUser)
			},
			bucket:   func(r jmapcore.SetResponse, string2 string) *jmapcore.SetError { return r.NotUpdated["nosuchid"] },
			wantType: jmapcore.SetErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)
			resp := emailSetCall(t, s, tc.args(id))

			serr := tc.bucket(resp, id)
			if serr == nil {
				t.Fatalf("the call reported no failure for an operation that is not built: %+v", resp)
			}
			if serr.Type != tc.wantType {
				t.Errorf("SetError type = %q, want %q (%s)", serr.Type, tc.wantType, serr.Description)
			}
			if len(resp.Updated) != 0 || len(resp.Created) != 0 || len(resp.Destroyed) != 0 {
				t.Errorf("something was reported as done: %+v", resp)
			}
			// And the store is untouched: the message keeps the flags it was
			// delivered with.
			flags, keywords := storedFlags(t, home)
			if !equalStrings(flags, []string{`\Seen`}) || len(keywords) != 0 {
				t.Errorf("the store changed on a refused call: flags=%v keywords=%v", flags, keywords)
			}
		})
	}
}

// ifInState is the client saying what it believes; answering a mismatch with
// success would let it write over a change it has not seen.
func TestEmailSetHonoursIfInState(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)

	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["Email/set",{"accountId":%q,"ifInState":"999","update":{%q:{"keywords":{}}}},"c0"]]}`,
		testUser, id)
	w := postAPIRaw(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), jmapcore.ErrStateMismatch) {
		t.Errorf("a stale ifInState was not refused: %s", w.Body)
	}
	if flags, _ := storedFlags(t, home); !equalStrings(flags, []string{`\Seen`}) {
		t.Errorf("the write went through on a state mismatch: %v", flags)
	}

	// The matching state still works, or the check would be a way of refusing
	// everything. It is read from the server rather than written as a literal:
	// a test that hard-codes the state passes only while the encoding stands
	// still, which is the opposite of what a versioned format is for.
	current := emailStateOf(t, s)
	resp := emailSetCall(t, s,
		fmt.Sprintf(`{"accountId":%q,"ifInState":%q,"update":{%q:{"keywords":{}}}}`, testUser, current, id))
	if _, ok := resp.Updated[id]; !ok {
		t.Errorf("a matching ifInState was refused: %+v", resp.NotUpdated[id])
	}
}

// Every keyword that can be read has to be writable, and the reverse. The two
// directions are separate tables in the code on purpose; this walks the pair,
// so a keyword added to one and forgotten in the other fails here rather than
// in a client.
func TestSystemKeywordMappingIsSymmetric(t *testing.T) {
	for kw, flag := range jmapToIMAPFlag {
		meta := &mailbox.MessageMeta{Flags: []string{flag}}
		if got := keywordsOf(meta); !got[kw] {
			t.Errorf("flag %q is written for %q but does not read back as it: %v", flag, kw, got)
		}
	}
	// And nothing readable is unwritable: keywordsOf's system table must have
	// no entry this one lacks.
	for _, flag := range []string{`\Seen`, `\Answered`, `\Flagged`, `\Draft`, `\Deleted`} {
		kws := keywordsOf(&mailbox.MessageMeta{Flags: []string{flag}})
		for kw := range kws {
			if _, ok := jmapToIMAPFlag[kw]; !ok {
				t.Errorf("keyword %q is readable but cannot be written back", kw)
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// raceBackend hands out an index that performs one concurrent write between the
// lookup and the JMAP write, which is the window the patch form has to survive.
// Without it the race is unreproducible in a single-session test, and the
// defect it guards against would only ever show up in production.
type raceBackend struct {
	mailbox.IndexBackend
	info    *mailbox.UserInfo
	once    bool
	inject  func(mailbox.UserIndex)
	tracked *raceIndex
}

type raceIndex struct {
	mailbox.UserIndex
	parent *raceBackend
}

func (b *raceBackend) OpenUser(info *mailbox.UserInfo) mailbox.UserIndex {
	idx := &raceIndex{UserIndex: b.IndexBackend.OpenUser(info), parent: b}
	b.tracked = idx
	return idx
}

func (i *raceIndex) UpdateFlagsMulti(folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsResult, error) {
	if !i.parent.once {
		i.parent.once = true
		i.parent.inject(i.UserIndex)
	}
	return i.UserIndex.UpdateFlagsMulti(folderID, updates)
}

// The lost-update the store closed for IMAP (#1282), asked of JMAP: another
// session adds a keyword between this call's lookup and its write. A patch is
// the client saying "add $seen", not "the set is now exactly what I last saw",
// so the concurrent keyword must survive -- and it only does if the patch
// reaches the store as a delta the store folds under its own lock.
func TestEmailSetPatchDoesNotEraseAConcurrentKeyword(t *testing.T) {
	tests := []struct {
		name         string
		patch        string
		wantFlags    []string
		wantKeywords []string
	}{
		{
			// The most common operation there is, and the one that used to be
			// an absolute write against a stale read.
			name:         "adding a keyword keeps one added meanwhile",
			patch:        `{"keywords/$answered":true}`,
			wantFlags:    []string{`\Answered`, `\Flagged`, `\Seen`},
			wantKeywords: nil,
		},
		{
			name:         "removing a keyword keeps one added meanwhile",
			patch:        `{"keywords/$seen":null}`,
			wantFlags:    []string{`\Flagged`},
			wantKeywords: nil,
		},
		{
			// The whole-object form is the client saying "the set is now
			// this", so losing the concurrent keyword is what it asked for.
			// The row is here so the difference between the two forms is
			// asserted rather than assumed.
			name:         "a whole-object replacement is still a replacement",
			patch:        `{"keywords":{"$draft":true}}`,
			wantFlags:    []string{`\Draft`},
			wantKeywords: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)
			info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
			// The concurrent writer: a second index handle over the same files,
			// adding \Flagged the way another session's STORE would.
			rb := &raceBackend{IndexBackend: file.New(), info: info}
			rb.inject = func(mailbox.UserIndex) {
				other := file.New().OpenUser(info)
				defer other.Close() //nolint:errcheck
				f, err := other.OpenFolder("INBOX", 0)
				if err != nil {
					t.Fatalf("concurrent open: %v", err)
				}
				if _, err := other.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
					1: {Mode: mailbox.FlagsAdd, Flags: []string{`\Flagged`}},
				}); err != nil {
					t.Fatalf("concurrent store: %v", err)
				}
			}
			s.opts.Storage.Index = rb

			resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:%s}}`, testUser, id, tc.patch))
			if len(resp.NotUpdated) != 0 {
				t.Fatalf("update refused: %+v", resp.NotUpdated[id])
			}

			flags, keywords := storedFlags(t, home)
			if !equalStrings(flags, tc.wantFlags) {
				t.Errorf("stored flags = %v, want %v -- a concurrent write was overwritten", flags, tc.wantFlags)
			}
			if !equalStrings(keywords, tc.wantKeywords) {
				t.Errorf("stored keywords = %v, want %v", keywords, tc.wantKeywords)
			}
		})
	}
}

// Giving a property directly and by patch in one update says two different
// things about the same set, and RFC 8620 §5.3 refuses it rather than picking
// an order.
func TestEmailSetRefusesAPropertyGivenTwice(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, setTestMessage, 0)
	resp := emailSetCall(t, s,
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords":{"$draft":true},"keywords/$seen":true}}}`, testUser, id))

	serr := resp.NotUpdated[id]
	if serr == nil {
		t.Fatalf("an update giving keywords twice was accepted: %+v", resp)
	}
	if serr.Type != jmapcore.SetErrInvalidPatch {
		t.Errorf("SetError type = %q, want %q", serr.Type, jmapcore.SetErrInvalidPatch)
	}
	if flags, _ := storedFlags(t, home); !equalStrings(flags, []string{`\Seen`}) {
		t.Errorf("the store changed on a refused update: %v", flags)
	}
}

// emailStateOf asks the server for the Email state the way a client would.
func emailStateOf(t *testing.T, s *Server) string {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["Email/get",{"accountId":%q,"ids":[]},"c0"]]}`, testUser)
	w := postAPIRaw(t, s, body)
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var got struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &got); err != nil {
		t.Fatalf("decode Email/get: %v -- %s", err, w.Body)
	}
	return got.State
}
