package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Row: IMAP ↔ JMAP, message identity. One delivered message is read through
// both protocols and the two readings are handed to the judge: EMAILID against
// Email.id, subject, size, internal date.
//
// The subject is deliberately a MIME encoded-word carrying non-ASCII. That is
// the input that separates a decoder from a pass-through, and it is the defect
// this area was opened for: a surface comparing decoded headers against
// undecoded ones shipped green because nothing read the same message twice
// (#1209).
const consistencySubjectRaw = "=?utf-8?Q?Rechnung_f=C3=BCr_M=C3=A4rz_=E2=82=AC42?="

// consistencySubjectDecoded is what a surface that decodes headers must report.
const consistencySubjectDecoded = "Rechnung für März €42"

func checkConsistencyIdentity(user, pass string) error {
	marker := consistencyMarker("identity")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	left, err := imapReadProbe(user, pass, marker)
	if err != nil {
		return fmt.Errorf("read over imap: %w", err)
	}
	right, err := jmapReadProbe(marker)
	if err != nil {
		return fmt.Errorf("read over jmap: %w", err)
	}
	return judgeRow("imap<->jmap identity", left, right, defaultAllowances())
}

// consistencyMarker is the per-run token that ties one delivery to the readings
// of it. In the subject rather than only the body: both surfaces can search a
// header without a full-text index, so this row does not depend on fts being
// deployed or caught up.
func consistencyMarker(row string) string {
	return fmt.Sprintf("xconsistency-%s-%d", row, time.Now().UnixNano())
}

func deliverConsistencyProbe(user, marker string) error {
	subject := consistencySubjectRaw + " " + marker
	if err := lmtpSend(uniqueID(), "consistency@test.invalid", user, subject,
		"the consistency probe body\r\n"); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}
	return nil
}

// imapReadProbe reads the probe as IMAP describes it. Values are taken as IMAP
// spells them — NIL stays NIL, the internal date keeps its layout — because
// normalising here is how a difference is hidden before the judge sees it.
func imapReadProbe(user, pass, marker string) (*reading, error) {
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
	lines, err := c.cmd(fmt.Sprintf("UID FETCH %s (EMAILID RFC822.SIZE INTERNALDATE ENVELOPE)", uid))
	if err != nil {
		return nil, fmt.Errorf("uid fetch: %w", err)
	}
	body := strings.Join(lines, " ")
	r := newReading(surfIMAP)
	if id := betweenTokens(body, "EMAILID (", ")"); id != "" {
		r.field("id", id)
	}
	if size := afterToken(body, "RFC822.SIZE "); size != "" {
		r.field("size", size)
	}
	if d := betweenTokens(body, `INTERNALDATE "`, `"`); d != "" {
		r.field("internalDate", d)
	}
	if s := envelopeSubject(body); s != "" {
		r.field("subject", s)
	}
	if len(r.fields) < 4 {
		return nil, fmt.Errorf("incomplete FETCH reading %v from %q", r.fields, body)
	}
	return r, nil
}

// waitForIMAPProbe waits for the delivery to become visible. The budget is the
// delivery one, not an index one: SEARCH HEADER does not go through fts.
func waitForIMAPProbe(c *imapClient, marker string) (string, error) {
	deadline := time.Now().Add(*flagTimeout * 3)
	for {
		uids, err := c.uidSearch("HEADER SUBJECT " + marker)
		if err != nil {
			return "", fmt.Errorf("uid search: %w", err)
		}
		if len(uids) == 1 {
			return uids[0], nil
		}
		if len(uids) > 1 {
			return "", fmt.Errorf("%d messages carry the marker %s; the probe is not identifiable", len(uids), marker)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the probe never became visible over imap (marker %s)", marker)
		}
		time.Sleep(500 * time.Millisecond)
		if _, err := c.selectFolder("INBOX"); err != nil {
			return "", fmt.Errorf("reselect INBOX: %w", err)
		}
	}
}

// jmapReadProbe reads the same message as JMAP describes it.
func jmapReadProbe(marker string) (*reading, error) {
	id, err := jmapProbeID(marker)
	if err != nil {
		return nil, err
	}
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":["` + id + `"],` +
		`"properties":["id","subject","size","receivedAt"]},"c0"]]}`)
	if err != nil {
		return nil, fmt.Errorf("Email/get: %w", err)
	}
	var resp struct {
		List []struct {
			ID         string `json:"id"`
			Subject    string `json:"subject"`
			Size       int64  `json:"size"`
			ReceivedAt string `json:"receivedAt"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode Email/get: %w", err)
	}
	if len(resp.List) != 1 {
		return nil, fmt.Errorf("Email/get returned %d messages for id %s", len(resp.List), id)
	}
	m := resp.List[0]
	return newReading(surfJMAP).
		field("id", m.ID).
		field("size", fmt.Sprintf("%d", m.Size)).
		field("internalDate", m.ReceivedAt).
		field("subject", m.Subject), nil
}

// jmapProbeID finds the probe by its marker, waiting the same budget the IMAP
// side waits: the two surfaces see a delivery at their own pace, and a row that
// gave up on one of them would report a disagreement that is a race.
func jmapProbeID(marker string) (string, error) {
	deadline := time.Now().Add(*flagTimeout * 3)
	for {
		raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
			`"methodCalls":[["Email/query",{"accountId":"` + *flagJMAPUser + `",` +
			`"filter":{"subject":"` + marker + `"}},"c0"]]}`)
		if err == nil {
			var resp struct {
				IDs []string `json:"ids"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return "", fmt.Errorf("decode Email/query: %w", err)
			}
			switch {
			case len(resp.IDs) == 1:
				return resp.IDs[0], nil
			case len(resp.IDs) > 1:
				return "", fmt.Errorf("%d messages carry the marker %s over jmap", len(resp.IDs), marker)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the probe never became visible over jmap (marker %s): %v", marker, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

/* --- FETCH scraping ------------------------------------------------------- */

func betweenTokens(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func afterToken(s, token string) string {
	i := strings.Index(s, token)
	if i < 0 {
		return ""
	}
	return strings.Fields(s[i+len(token):])[0]
}

// envelopeSubject pulls the subject out of an ENVELOPE. The second field, as
// RFC 3501 orders them: date first, subject second. Returned exactly as IMAP
// sent it — encoded-word and all — so the judge, not the reader, decides
// whether that spells the same subject the other surface reported.
func envelopeSubject(body string) string {
	i := strings.Index(body, "ENVELOPE (")
	if i < 0 {
		return ""
	}
	rest := body[i+len("ENVELOPE ("):]
	// date
	_, rest = imapQuoted(rest)
	subject, _ := imapQuoted(rest)
	return subject
}

// imapQuoted reads the next quoted string, literal or NIL, and returns it with
// the remainder of the input.
//
// Literals are not optional to support: a server may answer any string that
// way, and a subject carrying an encoded-word is long enough to make it likely.
// A reader that understood quoted strings only reported no subject at all and
// the row failed while the server was answering correctly (#1279).
func imapQuoted(s string) (value, rest string) {
	s = strings.TrimLeft(s, " ")
	if strings.HasPrefix(s, "NIL") {
		return "NIL", strings.TrimPrefix(s, "NIL")
	}
	if strings.HasPrefix(s, "{") {
		if end := strings.Index(s, "}"); end > 0 {
			n, err := strconv.Atoi(s[1:end])
			// cmd() joins the response lines with a single space, so the
			// literal's bytes start right after the brace and that separator.
			if err == nil && n >= 0 {
				body := strings.TrimPrefix(s[end+1:], " ")
				if n <= len(body) {
					return body[:n], body[n:]
				}
				return body, ""
			}
		}
		return "", s
	}
	if !strings.HasPrefix(s, `"`) {
		return "", s
	}
	s = s[1:]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), ""
}
