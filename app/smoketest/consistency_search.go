package main

import (
	"fmt"
	"strings"
	"time"
)

// Row: IMAP SEARCH ↔ JMAP Email/query over one term. The agreement is the SET
// of messages, not the ranking and not the ids — the two surfaces name messages
// differently, so each side's hits are read back as the per-message marker that
// identifies them across protocols.
//
// The set is seeded by this row and is small and fixed: judging against
// whatever the mailbox holds after the other areas ran would compare two
// surfaces over a corpus neither of them controls, and a difference would be a
// race as often as a defect.
//
// The corpus is chosen to distinguish, not to pass:
//
//   - one message carries the term in the SUBJECT only — the input that catches
//     a query implemented over the body alone, which is the defect this area
//     was opened for (#1209);
//   - one carries it in the BODY only;
//   - one carries neither, so a surface that answers "everything" is refused.
func checkConsistencySearch(user, pass string) error {
	// Short enough to sit well inside the tokenizer's length cap (30 bytes by
	// default): a term that lands exactly on the boundary is indexed and
	// queried by different rules on either side of it, and a row that fails
	// there says nothing about the surfaces it compares (#1279).
	//
	// Twelve digits, not six: the term also has to be unique across runs, and
	// a six-digit tail repeats within seconds on a busy sandbox — a row that
	// finds a previous run's message is a false pass. 17 bytes total, still
	// far inside the cap.
	term := fmt.Sprintf("xterm%d", time.Now().UnixNano()%1_000_000_000_000)
	inSubject := consistencyMarker("search-subj")
	inBody := consistencyMarker("search-body")
	neither := consistencyMarker("search-none")

	seed := []struct{ marker, subject, body string }{
		{inSubject, consistencySubjectRaw + " " + inSubject + " " + term, "no term in this body\r\n"},
		{inBody, consistencySubjectRaw + " " + inBody, "the term is here: " + term + "\r\n"},
		{neither, consistencySubjectRaw + " " + neither, "neither here\r\n"},
	}
	for _, m := range seed {
		if err := lmtpSend(uniqueID(), "consistency@test.invalid", user, m.subject, m.body); err != nil {
			return fmt.Errorf("seed %s: %w", m.marker, err)
		}
	}

	want := []string{inSubject, inBody}
	left, err := imapSearchMarkers(user, pass, term, len(want))
	if err != nil {
		return fmt.Errorf("search over imap: %w", err)
	}
	right, err := jmapQueryMarkers(term, len(want))
	if err != nil {
		return fmt.Errorf("query over jmap: %w", err)
	}
	// Both sides are judged against the seeded expectation first: two surfaces
	// that are wrong the same way agree, and agreement is not the property this
	// row is for.
	expected := newReading("seeded").set("search", want)
	if err := judgeRow("imap SEARCH against the seeded set", expected, left, defaultAllowances()); err != nil {
		return err
	}
	if err := judgeRow("jmap Email/query against the seeded set", expected, right, defaultAllowances()); err != nil {
		return err
	}
	return judgeRow("imap SEARCH <-> jmap Email/query", left, right, defaultAllowances())
}

// imapSearchMarkers runs one TEXT search and reads each hit back as its marker.
// Waits for the index to catch up on the same budget the FTS checks use: a set
// read before indexing settles is a race, not a disagreement.
func imapSearchMarkers(user, pass, term string, want int) (*reading, error) {
	c, err := imapDial()
	if err != nil {
		return nil, err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	deadline := time.Now().Add(*flagIMAPReadTimeout)
	for {
		if _, err := c.selectFolder("INBOX"); err != nil {
			return nil, fmt.Errorf("select INBOX: %w", err)
		}
		uids, err := c.uidSearch("TEXT " + term)
		if err != nil {
			return nil, fmt.Errorf("uid search: %w", err)
		}
		if len(uids) >= want || time.Now().After(deadline) {
			markers, err := imapMarkersOf(c, uids)
			if err != nil {
				return nil, err
			}
			return newReading(surfIMAP).set("search", markers), nil
		}
		time.Sleep(time.Second)
	}
}

func imapMarkersOf(c *imapClient, uids []string) ([]string, error) {
	var out []string
	for _, uid := range uids {
		lines, err := c.cmd("UID FETCH " + uid + " (ENVELOPE)")
		if err != nil {
			return nil, fmt.Errorf("uid fetch envelope: %w", err)
		}
		if m := markerIn(envelopeSubject(strings.Join(lines, " "))); m != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

func jmapQueryMarkers(term string, want int) (*reading, error) {
	deadline := time.Now().Add(*flagIMAPReadTimeout)
	for {
		ids, errType, err := jmapQueryText(*flagJMAPUser, term)
		if err != nil {
			return nil, err
		}
		if errType != "" {
			return nil, fmt.Errorf("Email/query refused the text filter: %s", errType)
		}
		if len(ids) >= want || time.Now().After(deadline) {
			var markers []string
			for _, id := range ids {
				subject, err := jmapSubjectOf(*flagJMAPUser, id)
				if err != nil {
					return nil, err
				}
				if m := markerIn(subject); m != "" {
					markers = append(markers, m)
				}
			}
			return newReading(surfJMAP).set("search", markers), nil
		}
		time.Sleep(time.Second)
	}
}

// markerIn pulls this area's marker out of a subject, whichever way the surface
// spelled the rest of it: the marker is plain ASCII, so it survives both the
// encoded-word and the decoded rendering.
func markerIn(subject string) string {
	for _, f := range strings.Fields(subject) {
		if strings.HasPrefix(f, "xconsistency-") {
			return f
		}
	}
	return ""
}
