package maildir

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Each acquisition is counted under the call that took it, not under one name
// for all of them.
//
// The point of the label is to tell this driver's share of a key it shares with
// the index apart from the index's own -- and to tell a delivery from a poll
// inside that share (#1630). A single total answers neither question, so the
// test asserts the split rather than the sum.
func TestLockAcquisitionsAreCountedByCaller(t *testing.T) {
	box, _ := batchBox(t)
	body := "From: a@b\r\n\r\nx\r\n"

	before := map[string]float64{}
	for _, site := range []string{lockSiteSave, lockSiteWriteFlagsBulk, lockSiteCreate} {
		before[site] = testutil.ToFloat64(metricLockAcquired.WithLabelValues(site))
	}

	if _, _, _, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Work"); err != nil {
		t.Fatal(err)
	}
	msgs, err := box.List("INBOX")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list = %v, err = %v", msgs, err)
	}
	box.WriteFlagsMulti("INBOX", []mailbox.FlagWrite{
		{UID: 1, Filename: msgs[0].Filename, Flags: []string{`\Seen`}},
	})

	for site, want := range map[string]float64{
		lockSiteSave:           1,
		lockSiteCreate:         1,
		lockSiteWriteFlagsBulk: 1,
	} {
		got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(site)) - before[site]
		if got != want {
			t.Errorf("site %q counted %v acquisitions, want %v", site, got, want)
		}
	}
	// And nothing landed under a site that did no work.
	if got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteMove)); got != 0 {
		t.Errorf("site %q counted %v acquisitions with no move done", lockSiteMove, got)
	}
}
