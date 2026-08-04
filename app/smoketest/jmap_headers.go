package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// smokeFolder is where this check does its work.
//
// Its own folder rather than INBOX: the smoke test runs on every rollout, and a
// message left behind changes the mailbox every other measurement is taken
// against — the same harm that keeps -delete off by default in the load
// generator, arriving more slowly. It also means a failed cleanup leaves one
// named trace instead of messages scattered through a real mailbox.
const smokeFolder = "YariloSmoke"

// headerFormsMessage is what the check appends.
//
// Every field here is one the check reads back, and each is chosen for a form
// that has somewhere to go wrong: a folded Received, a repeated one, an
// RFC 2047 subject, an address list mixing a group with plain addresses, and a
// display name containing an escaped quote.
var headerFormsMessage = strings.Join([]string{
	`Subject: =?utf-8?q?caf=C3=A9?=`,
	`From: "say \"hi\"" <sender@example.com>`,
	`To: Team:alice@example.com,bob@example.com;, carol@example.com`,
	`Date: Tue, 05 Aug 2026 09:30:00 +0200`,
	`Message-Id: <smoke-header-forms@yarilo.invalid>`,
	`List-Unsubscribe: <https://example.com/u/1>, <mailto:un@example.com>`,
	`X-Spam-Status: No, score=-2.1 required=5.0`,
	"Received: from a.example.com\r\n by b.example.com;\r\n Tue, 05 Aug 2026 09:29:00 +0200",
	`Received: from c.example.com by d.example.com`,
	``,
	`smoke test body`,
	``,
}, "\r\n")

// checkJMAPHeaderForms appends a message and reads it back through every
// header:* form of RFC 8621 §4.1.3, then checks that a misspelled form is
// refused rather than silently dropped.
//
// It brings its own message because the alternative is depending on whatever
// happens to be in the mailbox, which differs per deployment and makes a green
// run mean different things in different places.
func checkJMAPHeaderForms() error {
	if err := assertCRLF(headerFormsMessage); err != nil {
		return err
	}

	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	defer c.close()
	if err := c.login(*flagJMAPUser, *flagJMAPPass); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	if _, err := c.cmd(fmt.Sprintf("CREATE %q", smokeFolder)); err != nil &&
		!strings.Contains(strings.ToUpper(err.Error()), "ALREADYEXISTS") {
		return fmt.Errorf("CREATE %q: %w", smokeFolder, err)
	}
	// Cleanup is best effort and reported, not silent: leaving the folder
	// behind is recoverable, and not knowing it was left behind is not.
	defer func() {
		if cerr := cleanupSmokeFolder(c); cerr != nil {
			fmt.Printf("  ! cleanup: %v\n", cerr)
		}
	}()

	if err := c.append(smokeFolder, headerFormsMessage); err != nil {
		return fmt.Errorf("APPEND to %s: %w", smokeFolder, err)
	}

	mailboxID, err := jmapMailboxID(smokeFolder)
	if err != nil {
		return err
	}
	emailID, err := jmapNewestEmail(mailboxID)
	if err != nil {
		return err
	}
	if err := verifyHeaderForms(emailID); err != nil {
		return err
	}
	return verifyPropertyValidation(emailID)
}

// assertCRLF refuses a message that is not what the wire carries.
//
// The literal length an APPEND declares is a byte count, so a bare LF makes the
// message shorter on one side of the connection than the other. This has cost a
// day more than once through openssl's -crlf, which rewrites the bytes after
// the count was taken. Go does not rewrite anything, which is exactly why the
// next person to edit the message above with a Go raw string would reintroduce
// it — so it is asserted rather than assumed.
func assertCRLF(msg string) error {
	for i := 0; i < len(msg); i++ {
		if msg[i] == '\n' && (i == 0 || msg[i-1] != '\r') {
			return fmt.Errorf("the smoke message has a bare LF at byte %d; "+
				"the APPEND literal counts bytes, so it would declare a length the wire does not carry", i)
		}
	}
	return nil
}

func cleanupSmokeFolder(c *imapClient) error {
	if _, err := c.selectFolder(smokeFolder); err != nil {
		return fmt.Errorf("SELECT %s: %w", smokeFolder, err)
	}
	uids, err := c.uidSearch("ALL")
	if err != nil {
		return fmt.Errorf("UID SEARCH: %w", err)
	}
	if len(uids) > 0 {
		if err := c.deleteUIDs(uids); err != nil {
			return fmt.Errorf("expunge %d messages: %w", len(uids), err)
		}
	}
	// DELETE needs the folder unselected on a server that holds the lock.
	c.cmd("CLOSE") //nolint:errcheck
	c.deleteFolder(smokeFolder)
	return nil
}

// jmapMailboxID resolves a folder name to its JMAP id.
func jmapMailboxID(name string) (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Mailbox/get",{"accountId":"` + *flagJMAPUser + `","ids":null},"c0"]]}`)
	if err != nil {
		return "", err
	}
	// raw is the method's arguments already: jmapCall unwrapped the envelope.
	var get struct {
		List []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &get); err != nil {
		return "", fmt.Errorf("decode Mailbox/get: %w", err)
	}
	for _, mbox := range get.List {
		if mbox.Name == name {
			return mbox.ID, nil
		}
	}
	return "", fmt.Errorf("Mailbox/get does not list %q, which IMAP just created: %s", name, raw)
}

// jmapNewestEmail returns the id of the newest message in one mailbox.
func jmapNewestEmail(mailboxID string) (string, error) {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/query",{"accountId":"` + *flagJMAPUser + `","limit":1,` +
		`"filter":{"inMailbox":"` + mailboxID + `"},` +
		`"sort":[{"property":"receivedAt","isAscending":false}]},"c0"]]}`)
	if err != nil {
		return "", err
	}
	var query struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &query); err != nil {
		return "", fmt.Errorf("decode Email/query: %w", err)
	}
	if len(query.IDs) == 0 {
		return "", fmt.Errorf("Email/query found nothing in the folder IMAP just appended to: %s", raw)
	}
	return query.IDs[0], nil
}

// headerFormExpectation is one property and the JSON it must answer with.
type headerFormExpectation struct {
	property string
	want     string
}

// verifyHeaderForms reads every form back and compares it verbatim.
//
// Compared as JSON rather than by decoding into Go values: the type is half the
// answer. asURLs returning the raw string instead of an array is exactly the
// failure this exists to catch, and a check that decoded into an interface and
// looked for a substring would pass on it.
func verifyHeaderForms(emailID string) error {
	expectations := []headerFormExpectation{
		// The reason the forms exist: fields no data type models.
		{`header:List-Unsubscribe`, `" <https://example.com/u/1>, <mailto:un@example.com>"`},
		{`header:List-Unsubscribe:asURLs`, `["https://example.com/u/1","mailto:un@example.com"]`},
		{`header:X-Spam-Status`, `" No, score=-2.1 required=5.0"`},

		// Raw keeps what was written; text unfolds, decodes and trims it.
		{`header:Subject`, `" =?utf-8?q?caf=C3=A9?="`},
		{`header:Subject:asText`, `"café"`},

		// Structured forms.
		{`header:Message-Id:asMessageIds`, `["smoke-header-forms@yarilo.invalid"]`},
		{`header:Date:asDate`, `"2026-08-05T07:30:00Z"`},

		// An escaped quote in the display name must survive as a display name,
		// not break the parse.
		{`header:From:asAddresses`, `[{"name":"say \"hi\"","email":"sender@example.com"}]`},

		// A group and plain addresses in one field: three groups, and the ones
		// outside the group carry a null name.
		{`header:To:asGroupedAddresses`,
			`[{"name":null,"addresses":[]},` +
				`{"name":"Team","addresses":[{"name":null,"email":"alice@example.com"},` +
				`{"name":null,"email":"bob@example.com"}]},` +
				`{"name":null,"addresses":[{"name":null,"email":"carol@example.com"}]}]`},

		// Without :all the answer is the last occurrence; with it, every one in
		// message order. The folded one keeps its folding raw and loses it as
		// text.
		{`header:Received`, `" from c.example.com by d.example.com"`},
		{`header:Received:all`, `[" from a.example.com\r\n by b.example.com;\r\n Tue, 05 Aug 2026 09:29:00 +0200",` +
			`" from c.example.com by d.example.com"]`},
		{`header:Received:asText:all`, `["from a.example.com by b.example.com; Tue, 05 Aug 2026 09:29:00 +0200",` +
			`"from c.example.com by d.example.com"]`},

		// A field that is not there is answered, not omitted. Checked for
		// presence separately below, because in Go a missing key and a null
		// value are both nil and a value-only check passes against a server
		// that drops the property.
		{`header:X-Nonexistent`, `null`},
		{`header:X-Nonexistent:all`, `[]`},
	}

	props := make([]string, 0, len(expectations)+2)
	props = append(props, "id", "headers")
	for _, e := range expectations {
		props = append(props, e.property)
	}
	encoded, err := json.Marshal(props)
	if err != nil {
		return err
	}

	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":["` + emailID + `"],` +
		`"properties":` + string(encoded) + `},"c0"]]}`)
	if err != nil {
		return err
	}
	email, err := firstEmailObject(raw)
	if err != nil {
		return err
	}

	// headers is the whole list, and it had no field behind it until recently:
	// requesting it read the message and answered nothing.
	if err := verifyHeadersList(email); err != nil {
		return err
	}
	// The response must carry what was asked for and nothing else. An
	// unprojected answer states hasAttachment as false and preview as empty for
	// a message that has both — present, and wrong.
	for _, unwanted := range []string{"subject", "from", "hasAttachment", "preview", "textBody", "size"} {
		if _, present := email[unwanted]; present {
			return fmt.Errorf("%q is in the answer and was not requested; "+
				"an unrequested property is answered from a field this request never computed", unwanted)
		}
	}

	var failures []string
	for _, e := range expectations {
		got, present := email[e.property]
		if !present {
			failures = append(failures, fmt.Sprintf("%s: absent from the answer", e.property))
			continue
		}
		if canon := string(got); !sameJSON(canon, e.want) {
			failures = append(failures, fmt.Sprintf("%s: got %s, want %s", e.property, canon, e.want))
		}
	}
	if len(failures) > 0 {
		// Every mismatch at once: a rollout check that reports one form at a
		// time costs a deploy per failure.
		return fmt.Errorf("header forms:\n    %s", strings.Join(failures, "\n    "))
	}
	return nil
}

// verifyHeadersList checks the headers property: every field of the message, in
// the order it carries them.
//
// The order is the answer, not incidental — Received lines read as a route, and
// a sorted or deduplicated list is a different message.
func verifyHeadersList(email map[string]json.RawMessage) error {
	raw, present := email["headers"]
	if !present {
		return fmt.Errorf("headers is absent from the answer; it was requested, " +
			"and until it had a field behind it the request read the message and answered nothing")
	}
	var list []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("decode headers: %w", err)
	}
	want := []string{
		"Subject", "From", "To", "Date", "Message-Id",
		"List-Unsubscribe", "X-Spam-Status", "Received", "Received",
	}
	if len(list) != len(want) {
		return fmt.Errorf("headers has %d fields, want %d: %s", len(list), len(want), raw)
	}
	for i, name := range want {
		if list[i].Name != name {
			return fmt.Errorf("headers[%d] is %q, want %q — the order is the message's",
				i, list[i].Name, name)
		}
	}
	return nil
}

// verifyPropertyValidation checks that a misspelled property is refused.
//
// The pair a client cannot otherwise separate: a typo is fixed by editing the
// request, an unimplemented property by waiting. Silence answers both the same
// way, which is what this replaced.
func verifyPropertyValidation(emailID string) error {
	for _, property := range []string{
		"header:List-Unsubscribe:asURL", // a form that does not exist
		"subjekt",                       // a plain typo
	} {
		// jmapCallRaw, not jmapCall: the expected answer here IS a method
		// error, and jmapCall reports one as a failure of the call.
		name, args, err := jmapCallRaw(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
			`["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":["` + emailID + `"],` +
			`"properties":["id","` + property + `"]},"c0"]]}`)
		if err != nil {
			return err
		}
		if name != "error" {
			return fmt.Errorf("%q was accepted; an unknown property must be refused, "+
				"or a client cannot tell its own typo from a property yarilo has not implemented: %s",
				property, args)
		}
		var merr struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Arguments   []string `json:"arguments"`
		}
		if err := json.Unmarshal(args, &merr); err != nil {
			return fmt.Errorf("decode error object: %w", err)
		}
		if merr.Type != "invalidArguments" {
			return fmt.Errorf("%q refused with %q, want invalidArguments", property, merr.Type)
		}
		if !strings.Contains(merr.Description, property) {
			return fmt.Errorf("%q refused without being named: %q", property, merr.Description)
		}
	}
	return nil
}

// firstEmailObject pulls the single Email out of an Email/get response, as raw
// JSON per property.
func firstEmailObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var get struct {
		List []map[string]json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(raw, &get); err != nil {
		return nil, fmt.Errorf("decode Email/get: %w", err)
	}
	if len(get.List) != 1 {
		return nil, fmt.Errorf("Email/get returned %d objects, want 1: %s", len(get.List), raw)
	}
	return get.List[0], nil
}

// sameJSON compares two JSON documents by value, so key order and whitespace do
// not decide whether a rollout passes.
func sameJSON(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return jsonEqual(av, bv)
}

func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			other, present := bv[k]
			if !present || !jsonEqual(v, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
