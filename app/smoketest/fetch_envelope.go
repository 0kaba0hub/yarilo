package main

import (
	"fmt"
	"strings"
)

// fetchEnvelopeProbeSubject marks the message this check appends when it finds
// an empty INBOX, so cleanup removes exactly that one.
const fetchEnvelopeProbeSubject = "yarilo-smoke-envelope-probe"

// checkFetchEnvelope runs the command a mail client issues to draw a message
// list, on an account's REAL INBOX.
//
// The gate missed a crash in exactly this command because everything else
// reaches IMAP through mailboxes the run creates itself: a fresh mailbox
// carries the current index extensions, while an account that predates them
// takes a different path -- the one that was broken (#1184). So this check is
// only worth what the account is: point it at a long-lived user, never at one
// the smoke run provisions.
//
// Read-only by construction: ENVELOPE and BODYSTRUCTURE do not set \Seen, so
// a live mailbox is safe to probe. An empty INBOX gets a probe message that
// is removed afterwards.
func checkFetchEnvelope(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	return fetchEnvelopeProbe(c)
}

// fetchEnvelopeProbe is checkFetchEnvelope with the connection already open,
// so the failure shapes can be driven in tests.
func fetchEnvelopeProbe(c *imapClient) error {
	exists, err := c.selectFolder("INBOX")
	if err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	if exists == 0 {
		const probe = "From: smoke@yarilo.test\r\n" +
			"Subject: " + fetchEnvelopeProbeSubject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"sm0ke\"\r\n\r\n" +
			"--sm0ke\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
			"--sm0ke--\r\n"
		if err := c.append("INBOX", probe); err != nil {
			return fmt.Errorf("append probe to empty INBOX: %w", err)
		}
		defer func() {
			uids, serr := c.uidSearch("SUBJECT " + fetchEnvelopeProbeSubject)
			if serr == nil {
				_ = c.deleteUIDs(uids)
			}
		}()
		if exists, err = c.selectFolder("INBOX"); err != nil || exists == 0 {
			return fmt.Errorf("probe not visible in INBOX (exists=%d): %w", exists, err)
		}
	}

	// Twice: the first pass parses and fills the cache, the second is served
	// from it. A cache that answers wrongly, or a reader that dies on a cache
	// it just wrote, shows only on the second.
	for _, label := range []string{"cold", "warm"} {
		lines, ferr := c.cmd(fmt.Sprintf("FETCH %d (ENVELOPE BODYSTRUCTURE)", exists))
		if ferr != nil {
			return fmt.Errorf("%s FETCH (ENVELOPE BODYSTRUCTURE) on message %d: %w", label, exists, ferr)
		}
		var fetched string
		for _, l := range lines {
			if strings.HasPrefix(l, "* ") && strings.Contains(l, "FETCH") {
				fetched = l
				break
			}
		}
		if fetched == "" {
			return fmt.Errorf("%s FETCH returned no untagged data for message %d", label, exists)
		}
		for _, want := range []string{"ENVELOPE", "BODYSTRUCTURE"} {
			if !strings.Contains(fetched, want) {
				return fmt.Errorf("%s FETCH answered without %s: %s", label, want, fetched)
			}
		}
	}
	return nil
}
