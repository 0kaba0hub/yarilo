package main

import (
	"fmt"
	"time"
)

// checkFTS proves the full-text search pipeline end-to-end: delivers a
// message with a unique marker via LMTP, waits for the yarilo-fts autoindex
// worker to pick it up, then verifies SEARCH finds it by body content,
// header (subject), and envelope sender — the three query shapes FTS backs.
// A message that never becomes searchable, or is only findable through the
// sequential-scan fallback while fts_search_read_fallback is false, surfaces
// as a hard SEARCH error rather than a silent miss.
func checkFTS(user, pass string) error {

	marker := fmt.Sprintf("ftsmarker%d", time.Now().UnixNano())
	id := uniqueID()
	from := "fts-probe@test.invalid"
	subject := "fts smoke " + marker
	body := "the quick fts probe body " + marker + " end"

	if err := lmtpSend(id, from, user, subject, body); err != nil {
		return fmt.Errorf("lmtp deliver: %w", err)
	}

	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}

	deadline := time.Now().Add(*flagTimeout * 3)
	var lastErr error
	for time.Now().Before(deadline) {
		if uids, err := c.uidSearch(fmt.Sprintf("BODY %q", marker)); err != nil {
			lastErr = fmt.Errorf("SEARCH BODY: %w", err)
		} else if len(uids) > 0 {
			lastErr = nil
			break
		} else {
			lastErr = fmt.Errorf("SEARCH BODY %q: no match yet", marker)
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("indexing never completed: %w", lastErr)
	}

	if uids, err := c.uidSearch(fmt.Sprintf("TEXT %q", marker)); err != nil {
		return fmt.Errorf("SEARCH TEXT: %w", err)
	} else if len(uids) == 0 {
		return fmt.Errorf("SEARCH TEXT %q: no match", marker)
	}

	if uids, err := c.uidSearch(fmt.Sprintf("HEADER SUBJECT %q", marker)); err != nil {
		return fmt.Errorf("SEARCH HEADER SUBJECT: %w", err)
	} else if len(uids) == 0 {
		return fmt.Errorf("SEARCH HEADER SUBJECT %q: no match", marker)
	}

	if uids, err := c.uidSearch(fmt.Sprintf("FROM %q", from)); err != nil {
		return fmt.Errorf("SEARCH FROM: %w", err)
	} else if len(uids) == 0 {
		return fmt.Errorf("SEARCH FROM %q: no match", from)
	}

	// Negative control: a token that was never delivered must not match —
	// catches a stuck/stale index silently returning everything.
	if uids, err := c.uidSearch(fmt.Sprintf("BODY %q", marker+"-absent")); err != nil {
		return fmt.Errorf("SEARCH BODY (negative control): %w", err)
	} else if len(uids) != 0 {
		return fmt.Errorf("SEARCH BODY (negative control) matched %d messages, want 0", len(uids))
	}

	return nil
}
