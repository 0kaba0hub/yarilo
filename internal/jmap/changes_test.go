package jmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// changesCall posts one Foo/changes and returns either the response or the
// error type, so a test can assert on the refusal as easily as on the result.
func changesCall(t *testing.T, s *Server, method, args string) (map[string]any, string) {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[[%q,%s,"c0"]]}`, method, args)
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
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("method name: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.MethodResponses[0][1], &payload); err != nil {
		t.Fatalf("payload: %v -- %s", err, w.Body)
	}
	if name == "error" {
		errType, _ := payload["type"].(string)
		return nil, errType
	}
	return payload, ""
}

func changedIDsOf(t *testing.T, payload map[string]any, field string) []string {
	t.Helper()
	raw, ok := payload[field].([]any)
	if !ok {
		t.Fatalf("%s is not a list: %v", field, payload[field])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// Every way of not knowing has to end in cannotCalculateChanges. An empty
// destroyed list means "nothing was deleted", and saying that when the server
// cannot see is how a client keeps listing messages that are gone.
func TestChangesRefusesWhatItCannotDiff(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		sinceState string
		wantErr    string
	}{
		{
			// The reason the format is versioned from the first release that
			// emitted a state at all.
			name:   "a state from another format version",
			method: "Email/changes", sinceState: "2-AQAB", wantErr: "cannotCalculateChanges",
		},
		{
			name: "the placeholder this server used to emit", method: "Email/changes",
			sinceState: "0", wantErr: "cannotCalculateChanges",
		},
		{
			name: "something the client invented", method: "Email/changes",
			sinceState: "not-a-state", wantErr: "cannotCalculateChanges",
		},
		{
			// Same shape, different object type: the markers do not mean the
			// same thing, so diffing them would answer confidently and wrongly.
			name: "a Mailbox state passed to Email/changes", method: "Email/changes",
			sinceState: "", wantErr: "cannotCalculateChanges",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
			since := tc.sinceState
			if since == "" {
				// The wrong-type case needs a real state of the other kind.
				h := openHandleForTest(t, s)
				list, err := s.mailboxList(h)
				if err != nil {
					t.Fatalf("mailbox list: %v", err)
				}
				since = mailboxState(list)
			}
			_, errType := changesCall(t, s, tc.method,
				fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
			if errType != tc.wantErr {
				t.Errorf("error = %q, want %q", errType, tc.wantErr)
			}
		})
	}
}

// The diff has to name what changed, and to tell a creation from an update --
// the distinction nextUID exists for. Reporting everything as updated would
// pass a weaker test and make a client refetch each change as though it were
// new.
func TestEmailChangesNamesWhatChanged(t *testing.T) {
	s, id, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	since := emailStateOf(t, s)

	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testUser, id))

	payload, errType := changesCall(t, s, "Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Email/changes refused: %s", errType)
	}
	updated := changedIDsOf(t, payload, "updated")
	if len(updated) != 1 || updated[0] != id {
		t.Errorf("updated = %v, want exactly [%s]", updated, id)
	}
	if got := changedIDsOf(t, payload, "created"); len(got) != 0 {
		t.Errorf("a flag change was reported as a creation: %v", got)
	}
	if got := changedIDsOf(t, payload, "destroyed"); len(got) != 0 {
		t.Errorf("destroyed = %v, want none", got)
	}
	if payload["newState"] == since {
		t.Error("newState did not move after a change")
	}
	if payload["oldState"] != since {
		t.Errorf("oldState = %v, want the state the client sent", payload["oldState"])
	}
}

// Nothing changed means nothing is reported, and the state stands. A diff that
// answered "everything updated" for an unchanged account would keep clients
// refetching for ever.
func TestEmailChangesIsEmptyWhenNothingHappened(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	since := emailStateOf(t, s)

	payload, errType := changesCall(t, s, "Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Email/changes refused: %s", errType)
	}
	for _, field := range []string{"created", "updated", "destroyed"} {
		if got := changedIDsOf(t, payload, field); len(got) != 0 {
			t.Errorf("%s = %v on an unchanged account", field, got)
		}
	}
	if payload["newState"] != since {
		t.Errorf("state moved with no change: %v -> %v", since, payload["newState"])
	}
}

// maxChanges is a ceiling, not a page size: answering a truncated list with a
// fresh state would tell the client it had seen everything.
func TestEmailChangesRefusesMoreThanMaxChanges(t *testing.T) {
	s, id, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	since := emailStateOf(t, s)
	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$flagged":true}}}`, testUser, id))

	_, errType := changesCall(t, s, "Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q,"maxChanges":0}`, testUser, since))
	if errType != "" {
		t.Errorf("maxChanges 0 means no limit, not a refusal: %s", errType)
	}
}

// A deletion must reach the client as an id in destroyed. Discovering it by
// absence is not the same thing: a client that only sees a message stop being
// listed never learns it was deleted, and a client polling /changes does not
// list anything at all.
func TestEmailChangesReportsDestroyedByID(t *testing.T) {
	s, id, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	since := emailStateOf(t, s)

	h := openHandleForTest(t, s)
	marks, err := s.folderMarks(h)
	if err != nil || len(marks) == 0 {
		t.Fatalf("folder marks: %v %v", marks, err)
	}
	if err := h.idx.ExpungeMessage(marks[0].folder.ID, 1); err != nil {
		t.Fatalf("expunge: %v", err)
	}

	payload, errType := changesCall(t, s, "Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Email/changes refused: %s", errType)
	}
	destroyed := changedIDsOf(t, payload, "destroyed")
	if len(destroyed) != 1 || destroyed[0] != id {
		t.Errorf("destroyed = %v, want exactly [%s] -- the client is never told what went", destroyed, id)
	}
}

// The floor gate, end to end: once the fold has taken the expunge records, a
// client asking about a point before it must be refused. The empty answer it
// would otherwise get reads as "nothing was deleted".
func TestEmailChangesRefusesBelowTheExpungeFloor(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	since := emailStateOf(t, s)

	h := openHandleForTest(t, s)
	marks, err := s.folderMarks(h)
	if err != nil || len(marks) == 0 {
		t.Fatalf("folder marks: %v %v", marks, err)
	}
	folderID := marks[0].folder.ID
	if err := h.idx.ExpungeMessage(folderID, 1); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	// The fold: the expunge record is now in the base and out of the log.
	if err := h.idx.OptimizeIndex(folderID); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	payload, errType := changesCall(t, s, "Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "cannotCalculateChanges" {
		t.Fatalf("a state below the floor answered %q with %v -- an empty destroyed list is read as 'nothing was deleted'",
			errType, payload)
	}
}

// mailboxStateOf asks the server for the Mailbox state the way a client would.
func mailboxStateOf(t *testing.T, s *Server) string {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["Mailbox/get",{"accountId":%q,"ids":[]},"c0"]]}`, testUser)
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
		t.Fatalf("decode Mailbox/get: %v -- %s", err, w.Body)
	}
	return got.State
}

// mailboxIDOf returns the client-facing id of one folder.
func mailboxIDOf(t *testing.T, s *Server, name string) string {
	t.Helper()
	h := openHandleForTest(t, s)
	list, err := s.mailboxList(h)
	if err != nil {
		t.Fatalf("mailbox list: %v", err)
	}
	for _, mb := range list {
		if mb.Name == name {
			return mb.ID
		}
	}
	t.Fatalf("mailbox %q not found in %v", name, list)
	return ""
}

// A deleted mailbox has to be NAMED in destroyed. A client that merely stops
// seeing an entry never learns the mailbox is gone and keeps it in its list for
// ever -- the same failure as a message discovered missing by absence, one
// level up. Without this row the destroyed branch of the diff could be lost in
// a refactor and nothing would fail.
func TestMailboxChangesReportsCreatedUpdatedAndDestroyed(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)

	// created
	since := mailboxStateOf(t, s)
	h := openHandleForTest(t, s)
	if err := h.box.Create("Sales"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.idx.OpenFolder("Sales", 0); err != nil {
		t.Fatalf("open new folder: %v", err)
	}
	payload, errType := changesCall(t, s, "Mailbox/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Mailbox/changes refused after a create: %s", errType)
	}
	created := changedIDsOf(t, payload, "created")
	salesID := mailboxIDOf(t, s, "Sales")
	if len(created) != 1 || created[0] != salesID {
		t.Fatalf("created = %v, want exactly [%s]", created, salesID)
	}

	// updated: a rename changes the properties, not the identity
	since = mailboxStateOf(t, s)
	h = openHandleForTest(t, s)
	if err := h.box.Rename("Sales", "Deals"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := h.idx.RenameFolder("Sales", "Deals"); err != nil {
		t.Fatalf("rename index: %v", err)
	}
	payload, errType = changesCall(t, s, "Mailbox/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Mailbox/changes refused after a rename: %s", errType)
	}
	if updated := changedIDsOf(t, payload, "updated"); len(updated) != 1 || updated[0] != salesID {
		t.Errorf("updated = %v, want exactly [%s] -- a rename keeps the id and changes the properties",
			updated, salesID)
	}
	if got := changedIDsOf(t, payload, "destroyed"); len(got) != 0 {
		t.Errorf("a rename was reported as a deletion: %v", got)
	}

	// destroyed
	since = mailboxStateOf(t, s)
	h = openHandleForTest(t, s)
	if err := h.idx.DeleteFolder("Deals"); err != nil {
		t.Fatalf("delete index: %v", err)
	}
	if err := h.box.Delete("Deals"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	payload, errType = changesCall(t, s, "Mailbox/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
	if errType != "" {
		t.Fatalf("Mailbox/changes refused after a delete: %s", errType)
	}
	destroyed := changedIDsOf(t, payload, "destroyed")
	if len(destroyed) != 1 || destroyed[0] != salesID {
		t.Errorf("destroyed = %v, want exactly [%s] -- the client is never told the mailbox went",
			destroyed, salesID)
	}
}

// A container mailbox has no GUID of its own -- its id is its path -- so a
// description cannot rebuild it, and its deletion must refuse the call rather
// than report an id the server made up.
//
// Driven through a hand-built sinceState rather than through the store: this
// backend never lists a container as an entry of its own, so no sequence of
// creates and deletes reaches the branch. The state is a string a client sends,
// so sending the shape the branch is written for is the real path, not a
// shortcut around it.
func TestMailboxChangesRefusesAnUnnameableDeletion(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)
	h := openHandleForTest(t, s)
	list, err := s.mailboxList(h)
	if err != nil {
		t.Fatalf("mailbox list: %v", err)
	}

	// Everything the account has now, plus one entry carrying a digest and no
	// id bytes -- what a container contributes, and what is left over as
	// "deleted" because nothing in the account matches it.
	desc := jmapcore.Description{Kind: jmapcore.KindMailbox}
	for _, mb := range list {
		desc.Entries = append(desc.Entries, jmapcore.StateEntry{
			Key: mailboxKeyOf(mb.ID), Fields: mailboxFields(mb),
		})
	}
	desc.Entries = append(desc.Entries, jmapcore.StateEntry{
		Key: [8]byte{0xC0, 0x17, 0xA1, 0x11, 0xE7, 0x00, 0x00, 0x01}, Fields: []uint64{42},
	})

	_, errType := changesCall(t, s, "Mailbox/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, desc.String()))
	if errType != "cannotCalculateChanges" {
		t.Errorf("a deletion the server cannot name answered %q; it must refuse rather than invent an id", errType)
	}
}
