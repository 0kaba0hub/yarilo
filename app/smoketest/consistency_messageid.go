package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Row: IMAP ↔ JMAP, the identity of a message that arrived without one.
//
// A message delivered over LMTP with no Message-ID used to be stored without
// one, which is permanent: the header is part of the bytes, so nothing can add
// it later. Such a message is its own root in every conversation for ever, no
// reply can name it, and JMAP reports messageId as null.
//
// CI cannot see this. It is a property of what delivery writes, and the unit
// tests assert what prependHeaders returns -- neither of them opens the stored
// message afterwards. That is the class this area exists for (#1209).
func checkConsistencyMessageID(user, pass string) error {
	marker := consistencyMarker("messageid")
	if err := deliverProbeWithoutMessageID(user, marker); err != nil {
		return err
	}
	left, err := imapReadMessageID(user, pass, marker)
	if err != nil {
		return fmt.Errorf("read over imap: %w", err)
	}
	right, err := jmapReadMessageID(marker)
	if err != nil {
		return fmt.Errorf("read over jmap: %w", err)
	}
	if left.fields["messageId"] == "" || left.fields["messageId"] == "NIL" {
		return fmt.Errorf("delivered without a Message-ID and stored without one: imap says %q — "+
			"the message can never be replied to or threaded, and the header cannot be added afterwards",
			left.fields["messageId"])
	}
	return judgeRow("imap<->jmap message-id of a message delivered without one", left, right, defaultAllowances())
}

// deliverProbeWithoutMessageID sends a message whose header section has no
// Message-ID at all -- what an MTA-less deployment feeding LMTP directly, or a
// script, actually produces.
func deliverProbeWithoutMessageID(user, marker string) error {
	raw := "From: consistency@test.invalid\r\n" +
		"To: " + user + "\r\n" +
		"Subject: " + marker + "\r\n" +
		"Date: Mon, 01 Jan 2029 00:00:00 +0000\r\n" +
		"\r\n" +
		"delivered with no Message-ID\r\n"
	if strings.Contains(strings.ToLower(raw), "message-id:") {
		return fmt.Errorf("the probe carries a Message-ID; it would prove nothing")
	}
	if err := lmtpSendRaw("consistency@test.invalid", user, raw); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}
	return nil
}

func imapReadMessageID(user, pass, marker string) (*reading, error) {
	c, err := imapDial()
	if err != nil {
		return nil, err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return nil, fmt.Errorf("select INBOX: %w", err)
	}
	uid, err := waitForIMAPProbe(c, marker)
	if err != nil {
		return nil, err
	}
	lines, err := c.cmd(fmt.Sprintf("UID FETCH %s (ENVELOPE)", uid))
	if err != nil {
		return nil, fmt.Errorf("uid fetch: %w", err)
	}
	r := newReading(surfIMAP)
	r.field("messageId", envelopeMessageID(strings.Join(lines, " ")))
	return r, nil
}

// envelopeMessageID takes the last field of the ENVELOPE, which RFC 3501 §7.4.2
// defines as the message id. Read positionally rather than by pattern: a body
// or a subject can contain something that looks like one.
//
// The envelope's own closing paren is found by counting depth from its opening
// one. The last paren in the response closes the FETCH, not the ENVELOPE, and
// reading to it would return the id with a paren stuck to it -- and the address
// lists in between are nested parens, so a scan for the first close is wrong in
// the other direction.
//
// Quoting is honoured while counting, because a subject may contain a paren.
func envelopeMessageID(body string) string {
	start := strings.Index(body, "ENVELOPE (")
	if start < 0 {
		return ""
	}
	rest := body[start+len("ENVELOPE ("):]
	depth, inQuotes, end := 1, false, -1
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == '\\' && inQuotes:
			i++ // an escaped character inside a quoted string
		case c == '"':
			inQuotes = !inQuotes
		case inQuotes:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return ""
	}
	fields := strings.Fields(rest[:end])
	if len(fields) == 0 {
		return ""
	}
	// NIL is a value here, not a parse failure: it is how IMAP says the
	// message has no id, which is exactly the state this row exists to catch.
	return strings.Trim(fields[len(fields)-1], `"`)
}

func jmapReadMessageID(marker string) (*reading, error) {
	id, err := jmapProbeID(marker)
	if err != nil {
		return nil, err
	}
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":["` + id + `"],` +
		`"properties":["id","messageId"]},"c0"]]}`)
	if err != nil {
		return nil, fmt.Errorf("Email/get: %w", err)
	}
	ids, err := jmapMessageIDs(raw)
	if err != nil {
		return nil, err
	}
	r := newReading(surfJMAP)
	if len(ids) == 0 {
		// null, not absent: JMAP spells "this message has no identity" that
		// way, and the row must show it rather than report an empty reading.
		r.field("messageId", "null")
		return r, nil
	}
	// As JMAP spells it, brackets already stripped by the protocol. The
	// allowance holds the two spellings together; normalising here would hide
	// a difference before the judge saw it.
	r.field("messageId", ids[0])
	return r, nil
}

// jmapMessageIDs pulls Email/get's messageId list, which is an array of
// header values without angle brackets, or null.
func jmapMessageIDs(raw []byte) ([]string, error) {
	var resp struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.MethodResponses) == 0 {
		return nil, fmt.Errorf("Email/get returned no method response")
	}
	var call []json.RawMessage
	if err := json.Unmarshal(resp.MethodResponses[0], &call); err != nil || len(call) < 2 {
		return nil, fmt.Errorf("Email/get response is not a method call")
	}
	var args struct {
		List []struct {
			MessageID []string `json:"messageId"`
		} `json:"list"`
	}
	if err := json.Unmarshal(call[1], &args); err != nil {
		return nil, fmt.Errorf("decode Email/get list: %w", err)
	}
	if len(args.List) == 0 {
		return nil, fmt.Errorf("Email/get returned no message")
	}
	return args.List[0].MessageID, nil
}
