package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Row: JMAP state and changes, over a change this row makes itself.
//
// State and incremental changes are what phase 2 (#1216) is for, and nothing
// exercised them: `Email/changes` and `Mailbox/changes` were served by a
// released build and never called in the field. A client that cannot trust
// /changes resynchronises the whole account instead, which is the cost the
// feature exists to avoid — and it would do that silently, so the report has to
// ask.
//
// The input that distinguishes is the SECOND call: asking again from the state
// the first call returned must report nothing. A /changes that answers "here is
// everything" every time passes the first assertion and fails this one.
func checkConsistencyChanges(user, pass string) error {
	marker := consistencyMarker("changes")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	// Seen to begin with, so clearing $seen later moves unreadEmails: the
	// mailbox half needs a change that a counter is obliged to follow.
	if _, err := imapStoreFlags(user, pass, marker, `\Seen`); err != nil {
		return fmt.Errorf("prepare over imap: %w", err)
	}
	id, err := jmapProbeID(marker)
	if err != nil {
		return err
	}

	before, err := jmapEmailState()
	if err != nil {
		return err
	}

	// One change, made here, so the row is not reading somebody else's.
	if err := jmapSetKeyword(id, consistencyCustomFlag); err != nil {
		return fmt.Errorf("set keyword over jmap: %w", err)
	}

	updated, after, err := jmapEmailChanges(before)
	if err != nil {
		return err
	}
	if after == before {
		return fmt.Errorf("Email/changes returns the state it was given (%s) after a keyword was written: "+
			"a client would never learn that anything changed", before)
	}
	if !slices.Contains(updated, id) {
		return fmt.Errorf("Email/changes does not report %s as updated after its keyword was written; updated=%v", id, updated)
	}
	if len(updated) != 1 {
		return fmt.Errorf("Email/changes reports %d ids for one change (%v): a client would refetch messages that did not change", len(updated), updated)
	}

	// Asking again from the state just returned must report nothing. This is
	// the assertion a /changes that always answers "everything" fails.
	updatedAgain, _, err := jmapEmailChanges(after)
	if err != nil {
		return err
	}
	if len(updatedAgain) != 0 {
		return fmt.Errorf("Email/changes from the state it just returned still reports %v: the state does not advance, "+
			"so an incremental sync never converges", updatedAgain)
	}

	// Mailbox/changes, over a change that must move a counter.
	//
	// The first version of this asked for $flagged and then checked only that
	// newState was non-empty. Both halves were wrong: $flagged touches neither
	// totalEmails nor unreadEmails, so RFC 8621 does not require the mailbox
	// state to move at all -- the row could fail a correct server -- and "the
	// state is not empty" is satisfied by a server that returns the same state
	// every time, which is the shape this area exists to catch.
	//
	// Marking the message unread moves unreadEmails, so the counter changes
	// and the state must follow.
	inbox, err := jmapInboxID()
	if err != nil {
		return err
	}
	mailboxBefore, err := jmapMailboxState()
	if err != nil {
		return err
	}
	if err := jmapClearKeyword(id, "$seen"); err != nil {
		return fmt.Errorf("mark unread over jmap: %w", err)
	}
	mailboxUpdated, mailboxAfter, err := jmapMailboxChanges(mailboxBefore)
	if err != nil {
		return err
	}
	if mailboxAfter == mailboxBefore {
		return fmt.Errorf("Mailbox/changes returns the state it was given (%s) after unreadEmails changed", mailboxBefore)
	}
	if !slices.Contains(mailboxUpdated, inbox) {
		return fmt.Errorf("Mailbox/changes does not report the inbox (%s) as updated after unreadEmails changed; updated=%v", inbox, mailboxUpdated)
	}
	mailboxAgain, _, err := jmapMailboxChanges(mailboxAfter)
	if err != nil {
		return err
	}
	if len(mailboxAgain) != 0 {
		return fmt.Errorf("Mailbox/changes from the state it just returned still reports %v: the state does not advance", mailboxAgain)
	}
	return nil
}

func jmapEmailState() (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":[],"properties":["id"]},"c0"]]}`)
	if err != nil {
		return "", fmt.Errorf("Email/get for state: %w", err)
	}
	var got struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return "", fmt.Errorf("decode Email/get state: %w", err)
	}
	if got.State == "" {
		return "", fmt.Errorf("Email/get carries no state: %s", raw)
	}
	return got.State, nil
}

// jmapEmailChanges returns the ids Email/changes reports as updated since
// sinceState, and the state it hands back.
func jmapEmailChanges(sinceState string) ([]string, string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/changes",{"accountId":"` + *flagJMAPUser + `","sinceState":"` + sinceState + `"},"c0"]]}`)
	if err != nil {
		return nil, "", fmt.Errorf("Email/changes: %w", err)
	}
	var got struct {
		NewState string   `json:"newState"`
		Updated  []string `json:"updated"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, "", fmt.Errorf("decode Email/changes: %w", err)
	}
	return got.Updated, got.NewState, nil
}

func jmapMailboxState() (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Mailbox/get",{"accountId":"` + *flagJMAPUser + `","ids":[],"properties":["id"]},"c0"]]}`)
	if err != nil {
		return "", fmt.Errorf("Mailbox/get for state: %w", err)
	}
	var got struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return "", fmt.Errorf("decode Mailbox/get state: %w", err)
	}
	if got.State == "" {
		return "", fmt.Errorf("Mailbox/get carries no state: %s", raw)
	}
	return got.State, nil
}

// jmapMailboxChanges returns the mailbox ids reported as updated since
// sinceState, and the state handed back.
func jmapMailboxChanges(sinceState string) ([]string, string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Mailbox/changes",{"accountId":"` + *flagJMAPUser + `","sinceState":"` + sinceState + `"},"c0"]]}`)
	if err != nil {
		return nil, "", fmt.Errorf("Mailbox/changes: %w", err)
	}
	var got struct {
		NewState string   `json:"newState"`
		Updated  []string `json:"updated"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, "", fmt.Errorf("decode Mailbox/changes: %w", err)
	}
	return got.Updated, got.NewState, nil
}

// jmapInboxID is the mailbox the probe is delivered to.
func jmapInboxID() (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Mailbox/query",{"accountId":"` + *flagJMAPUser + `","filter":{"role":"inbox"}},"c0"]]}`)
	if err != nil {
		return "", fmt.Errorf("Mailbox/query for the inbox: %w", err)
	}
	var got struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return "", fmt.Errorf("decode Mailbox/query: %w", err)
	}
	if len(got.IDs) != 1 {
		return "", fmt.Errorf("role:inbox matched %d mailboxes, want 1", len(got.IDs))
	}
	return got.IDs[0], nil
}

// jmapClearKeyword removes a keyword, which is how the row moves a counter:
// unreadEmails changes when $seen does.
func jmapClearKeyword(id, keyword string) error {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/set",{"accountId":"` + *flagJMAPUser + `","update":{"` + id + `":` +
		`{"keywords/` + keyword + `":null}}},"c0"]]}`)
	if err != nil {
		return err
	}
	var resp struct {
		NotUpdated map[string]json.RawMessage `json:"notUpdated"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode Email/set: %w", err)
	}
	if len(resp.NotUpdated) > 0 {
		return fmt.Errorf("Email/set refused to clear %s: %v", keyword, resp.NotUpdated)
	}
	return nil
}
