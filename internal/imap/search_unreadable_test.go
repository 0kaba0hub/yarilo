package imap_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"
	"github.com/prometheus/client_golang/prometheus"
)

// A message the scan cannot read is not a message that matched: it was never
// looked at. go-imap's MatchMessage answers TRUE for a BODY/TEXT criterion when
// it is handed no message at all, and the read error was dropped -- so until
// #1283 every unreadable message matched every body search, silently. Both
// halves are pinned here: it must not match, and it must be counted.
//
// The counter is the assertion. A log line cannot be asserted without capturing
// the handler, and the metric is the part an operator alerts on anyway.
func TestSearchCountsMessagesItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	fake := &fakeFTS{stuck: true} // index behind: the scan is the only path
	c := startFTSTestServerIn(t, fake, false, root)

	appendBodyTo(t, c, "the needle is in this body")
	appendBodyTo(t, c, "another message with the needle too")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	before := counterValue(t)
	if uids := searchTextCrit(t, c, "needle"); len(uids) != 2 {
		t.Fatalf("baseline SEARCH TEXT = %v, want both messages", uids)
	}
	if got := counterValue(t) - before; got != 0 {
		t.Fatalf("readable messages counted as unreadable: %v", got)
	}

	// Take the stored bytes away under the session's feet, leaving the index
	// records in place: the shape a storage failure produces.
	removed := makeStoredMessagesUnreadable(t, root)
	if removed == 0 {
		t.Fatal("no stored messages were removed; the row would prove nothing")
	}

	before = counterValue(t)
	uids := searchTextCrit(t, c, "needle")
	if len(uids) != 0 {
		t.Fatalf("SEARCH matched %v over messages whose bytes cannot be read", uids)
	}
	if got := counterValue(t) - before; got != float64(removed) {
		t.Errorf("unreadable counter rose by %v, want %d — the answer excluded them silently",
			got, removed)
	}
}

func counterValue(t *testing.T) float64 {
	return unreadableCount(t, "search")
}

// unreadableCount reads one command's share of the shared counter. The label
// is part of the assertion: summing every series would let a count raised
// under the wrong command's name satisfy a row about this one.
func unreadableCount(t *testing.T, command string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "imap_unreadable_messages_total" {
			continue
		}
		var out float64
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "command" && l.GetValue() == command {
					out += m.GetCounter().GetValue()
				}
			}
		}
		return out
	}
	return 0
}

func makeStoredMessagesUnreadable(t *testing.T, root string) int {
	t.Helper()
	removed := 0
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil //nolint:nilerr // a vanished entry is not this walk's problem
		}
		switch filepath.Base(filepath.Dir(p)) {
		case "cur", "new":
			// chmod rather than unlink: a cached open descriptor would keep
			// serving a removed file, and the row would then prove nothing.
			if os.Chmod(p, 0) == nil {
				removed++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return removed
}

func searchTextCrit(t *testing.T, c *imapclient.Client, term string) []imap.UID {
	t.Helper()
	data, err := c.UIDSearch(&imap.SearchCriteria{Text: []string{term}}, nil).Wait()
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return data.AllUIDs()
}

func appendBodyTo(t *testing.T, c *imapclient.Client, body string) {
	t.Helper()
	raw := "From: s@x\r\nTo: user@test.com\r\nSubject: m\r\n\r\n" + body + "\r\n"
	ac := c.Append("INBOX", int64(len(raw)), nil)
	if _, err := ac.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
}

// The reason the fix cannot be "hand MatchMessage what we have": given no
// message it answers TRUE for a body criterion, so an unreadable message would
// match every body search. Pinned as a property of the matcher this code
// depends on — if it ever changes, the exclusion above stops being required and
// this row says so.
func TestMatcherTreatsAMissingMessageAsAMatch(t *testing.T) {
	crit := &imap.SearchCriteria{Text: []string{"needle"}}
	if !goimapserver.MatchMessage(1, imap.UID(1), time.Now(), 100, nil, nil, crit) {
		t.Skip("the matcher no longer matches on a missing message; the exclusion in Search can be revisited")
	}
}
