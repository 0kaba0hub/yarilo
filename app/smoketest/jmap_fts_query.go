package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// checkJMAPFTSQuery proves the full-text path of Email/query end to end: a
// message is delivered, then found by a text condition and read back by
// back-reference.
//
// The IMAP FTS check shares the engine underneath but not the JMAP translation
// above it, so a green gate without this one says only that nothing else broke
// (#1206).
// The account is the JMAP one throughout: the request authenticates as it, so
// delivering to a different -fts-user would query one account for a message
// that arrived in another. -fts-user is read as the deployment's statement
// that full-text search is configured at all.
func checkJMAPFTSQuery(user, _ string) error {
	marker := fmt.Sprintf("jmapftsmarker%d", time.Now().UnixNano())
	absent := marker + "notdelivered"
	subject := "jmap fts smoke " + marker
	if err := lmtpSend(uniqueID(), "jmap-fts-probe@test.invalid", user,
		subject, "the jmap fts probe body "+marker+" end"); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}

	return assertFTSQueryFindsOnly(user, marker, absent, subject)
}

// assertFTSQueryFindsOnly is the judgement, kept apart from the delivery above
// so it can be exercised without a cluster: the check is only as good as what
// it refuses, and what it refuses is here.
func assertFTSQueryFindsOnly(user, marker, absent, subject string) error {
	// Indexing is asynchronous; the same budget the IMAP check waits with.
	deadline := time.Now().Add(*flagTimeout * 3)
	var ids []string
	var lastErr error
	for time.Now().Before(deadline) {
		ids, lastErr = jmapQueryText(user, marker)
		if lastErr == nil && len(ids) > 0 {
			break
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("Email/query text %q: no match yet", marker)
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("indexing never completed: %w", lastErr)
	}
	// Exactly the delivered message: a query returning the whole mailbox also
	// contains the marker, and would satisfy "at least one hit" while proving
	// the opposite.
	if len(ids) != 1 {
		return fmt.Errorf("Email/query text %q returned %d messages, want exactly the delivered one", marker, len(ids))
	}
	subj, err := jmapSubjectOf(user, ids[0])
	if err != nil {
		return err
	}
	if subj != subject {
		return fmt.Errorf("Email/query matched a different message: subject %q, want %q", subj, subject)
	}

	// A marker that was never delivered must find nothing. Without this a
	// backend that ignores the condition and returns everything passes the
	// assertion above.
	other, err := jmapQueryText(user, absent)
	if err != nil {
		return fmt.Errorf("Email/query for an absent marker: %w", err)
	}
	if len(other) != 0 {
		return fmt.Errorf("Email/query text %q returned %d messages, want none: the condition is not being applied",
			absent, len(other))
	}
	return nil
}

// jmapQueryText runs Email/query with one text condition and returns the ids.
func jmapQueryText(user, text string) ([]string, error) {
	filter, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	args, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/query",{"accountId":"` + user + `","filter":` + string(filter) + `},"c0"]]}`)
	if err != nil {
		return nil, err
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(args, &out); err != nil {
		return nil, fmt.Errorf("decode Email/query: %w", err)
	}
	return out.IDs, nil
}

// jmapSubjectOf reads one message's subject, which is what identifies the hit
// as the message that was just delivered.
func jmapSubjectOf(user, id string) (string, error) {
	args, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/get",{"accountId":"` + user + `","ids":["` + id + `"],"properties":["id","subject"]},"c0"]]}`)
	if err != nil {
		return "", err
	}
	var out struct {
		List []struct {
			Subject string `json:"subject"`
		} `json:"list"`
	}
	if err := json.Unmarshal(args, &out); err != nil {
		return "", fmt.Errorf("decode Email/get: %w", err)
	}
	if len(out.List) != 1 {
		return "", fmt.Errorf("Email/get returned %d messages for one id", len(out.List))
	}
	return out.List[0].Subject, nil
}
