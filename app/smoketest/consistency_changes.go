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

	// Mailbox/changes over the same window: the flag write changes a mailbox's
	// counts, so its state must move too.
	mailboxBefore, err := jmapMailboxState()
	if err != nil {
		return err
	}
	if err := jmapSetKeyword(id, "$flagged"); err != nil {
		return fmt.Errorf("set second keyword over jmap: %w", err)
	}
	mailboxAfter, err := jmapMailboxChangesState(mailboxBefore)
	if err != nil {
		return err
	}
	if mailboxAfter == "" {
		return fmt.Errorf("Mailbox/changes returned no newState from %s", mailboxBefore)
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

func jmapMailboxChangesState(sinceState string) (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Mailbox/changes",{"accountId":"` + *flagJMAPUser + `","sinceState":"` + sinceState + `"},"c0"]]}`)
	if err != nil {
		return "", fmt.Errorf("Mailbox/changes: %w", err)
	}
	var got struct {
		NewState string `json:"newState"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return "", fmt.Errorf("decode Mailbox/changes: %w", err)
	}
	return got.NewState, nil
}
