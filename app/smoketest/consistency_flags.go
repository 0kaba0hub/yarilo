package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Row: IMAP ↔ JMAP, flags. A flag set over IMAP must be visible as the
// corresponding keyword, and a keyword set over JMAP must be visible as the
// flag — both directions, because a surface that only reads correctly is half
// a surface.
//
// The flag written is deliberately not a system one: a mapping table that
// handles \Seen and drops everything else answers the standard list correctly
// and loses a client's own labels, which is the shape a standard-flag-only row
// would call healthy.
const consistencyCustomFlag = "$smokelabel"

func checkConsistencyFlagsIMAPToJMAP(user, pass string) error {
	marker := consistencyMarker("flags")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	if _, err := imapStoreFlags(user, pass, marker, `\Seen `+consistencyCustomFlag); err != nil {
		return fmt.Errorf("set flags over imap: %w", err)
	}
	id, err := jmapProbeID(marker)
	if err != nil {
		return err
	}
	left := newReading(surfIMAP).set("flags", []string{`\Seen`, consistencyCustomFlag})
	right, err := jmapReadKeywords(id)
	if err != nil {
		return fmt.Errorf("read keywords over jmap: %w", err)
	}
	return judgeRow("imap->jmap flag visibility", left, right, defaultAllowances())
}

// The other direction, as its own row: a keyword written over JMAP has to be
// the flag IMAP reports.
//
// A row that always passes because its write silently did nothing reads in the
// report as a direction that was checked, which is why this was a named skip
// while Email/set did not exist. It exists now (#1216).
func checkConsistencyFlagsJMAPToIMAP(user, pass string) error {
	marker := consistencyMarker("flags-back")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	uid, err := imapStoreFlags(user, pass, marker, `\Seen`)
	if err != nil {
		return fmt.Errorf("prepare over imap: %w", err)
	}
	id, err := jmapProbeID(marker)
	if err != nil {
		return err
	}
	if err := jmapSetKeyword(id, "$flagged"); err != nil {
		return fmt.Errorf("set keyword over jmap: %w", err)
	}
	back, err := imapReadFlags(user, pass, uid)
	if err != nil {
		return fmt.Errorf("read flags over imap: %w", err)
	}
	// A custom keyword alongside the system one, because non-system keywords
	// are the vulnerable class -- losing one is what #1278 was, and a row that
	// only ever writes $flagged would not have seen it.
	if err := jmapSetKeyword(id, consistencyCustomFlag); err != nil {
		return fmt.Errorf("set custom keyword over jmap: %w", err)
	}
	back, err = imapReadFlags(user, pass, uid)
	if err != nil {
		return fmt.Errorf("read flags over imap: %w", err)
	}
	want := newReading(surfJMAP).set("flags", []string{"$seen", "$flagged", consistencyCustomFlag})
	return judgeRow("jmap->imap flag visibility", want, back, defaultAllowances())
}

func imapStoreFlags(user, pass, marker, flags string) (string, error) {
	c, err := imapDial()
	if err != nil {
		return "", err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return "", fmt.Errorf("select INBOX: %w", err)
	}
	uid, err := waitForIMAPProbe(c, marker)
	if err != nil {
		return "", err
	}
	if _, err := c.cmd(fmt.Sprintf("UID STORE %s +FLAGS (%s)", uid, flags)); err != nil {
		return "", fmt.Errorf("uid store: %w", err)
	}
	return uid, nil
}

func imapReadFlags(user, pass, uid string) (*reading, error) {
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
	lines, err := c.cmd("UID FETCH " + uid + " (FLAGS)")
	if err != nil {
		return nil, fmt.Errorf("uid fetch flags: %w", err)
	}
	flags := betweenTokens(strings.Join(lines, " "), "FLAGS (", ")")
	return newReading(surfIMAP).set("flags", storedFlags(strings.Fields(flags))), nil
}

// storedFlags drops \Recent, which RFC 3501 defines as session state rather
// than a stored flag: no other surface can report it, and comparing it would
// make the row fail on the session that first sees the message. Nothing else is
// filtered -- a flag dropped at read time is a difference nobody gets to judge.
func storedFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if strings.EqualFold(f, `\Recent`) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func jmapReadKeywords(id string) (*reading, error) {
	// The write went through IMAP; JMAP may be looking at a state it has not
	// re-read yet, so the same delivery budget applies here as everywhere.
	deadline := time.Now().Add(*flagTimeout * 3)
	for {
		raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
			`"methodCalls":[["Email/get",{"accountId":"` + *flagJMAPUser + `","ids":["` + id + `"],` +
			`"properties":["keywords"]},"c0"]]}`)
		if err != nil {
			return nil, fmt.Errorf("Email/get keywords: %w", err)
		}
		var resp struct {
			List []struct {
				Keywords map[string]bool `json:"keywords"`
			} `json:"list"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("decode keywords: %w", err)
		}
		if len(resp.List) != 1 {
			return nil, fmt.Errorf("Email/get returned %d messages for id %s", len(resp.List), id)
		}
		var keys []string
		for k, on := range resp.List[0].Keywords {
			if on {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 || time.Now().After(deadline) {
			return newReading(surfJMAP).set("flags", keys), nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func jmapSetKeyword(id, keyword string) error {
	raw, err := jmapCall(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Email/set",{"accountId":"` + *flagJMAPUser + `","update":{"` + id + `":` +
		`{"keywords/` + keyword + `":true}}},"c0"]]}`)
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
		return fmt.Errorf("Email/set refused the keyword: %v", resp.NotUpdated)
	}
	return nil
}
