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
func envelopeMessageID(body string) string {
	open := strings.Index(body, "ENVELOPE (")
	if open < 0 {
		return ""
	}
	rest := body[open+len("ENVELOPE ("):]
	// The message id is the last quoted string before the closing paren of the
	// envelope, and NIL when there is none.
	end := strings.LastIndex(rest, ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(rest[:end])
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	return strings.Trim(last, `"`)
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
	r.field("messageId", strings.Trim(ids[0], "<>"))
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
