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
	left, err := imapSearchMarkers(user, pass, term, append([]string{neither}, want...), len(want))
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

// imapSearchMarkers runs one TEXT search and reports which of the seeded
// markers it hit, by asking IMAP for each marker's UID rather than by reading
// subjects back.
//
// Reading them back was the defect: the probe subject is an encoded-word plus a
// marker, long enough that a server may return ENVELOPE as a literal rather
// than a quoted string. The scraper handled quoted strings only, so it found no
// markers, reported an empty IMAP set, and the row failed while both surfaces
// were answering correctly -- and it failed before it ever queried JMAP, which
// is why the JMAP side left no trace at all (#1279).
//
// A UID is what both searches already speak, so nothing has to be parsed.
func imapSearchMarkers(user, pass, term string, markers []string, want int) (*reading, error) {
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
		hits, err := c.uidSearch("TEXT " + term)
		if err != nil {
			return nil, fmt.Errorf("uid search: %w", err)
		}
		if len(hits) >= want || time.Now().After(deadline) {
			found, err := markersAmong(c, hits, markers)
			if err != nil {
				return nil, err
			}
			return newReading(surfIMAP).set("search", found), nil
		}
		time.Sleep(time.Second)
	}
}

// markersAmong maps the term's hits back to the markers that name them: each
// marker is searched for by header, and counted when its UID is among the hits.
func markersAmong(c *imapClient, hits []string, markers []string) ([]string, error) {
	inHits := make(map[string]bool, len(hits))
	for _, uid := range hits {
		inHits[uid] = true
	}
	var found []string
	for _, m := range markers {
		uids, err := c.uidSearch("HEADER SUBJECT " + m)
		if err != nil {
			return nil, fmt.Errorf("uid search for %s: %w", m, err)
		}
		for _, uid := range uids {
			if inHits[uid] {
				found = append(found, m)
				break
			}
		}
	}
	return found, nil
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
