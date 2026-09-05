package mdbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// A scan reads a record written at the size the file does not announce: the
// read path has since #1526, the scan stopped the folder for good (#1682).
func TestScanReadsARecordWrittenAtTheOtherSize(t *testing.T) {
	u := &userMailbox{}
	recs, err := u.scanMFileAt(filepath.Join("testdata", "m.shortheader"))
	if err != nil {
		t.Fatalf("scan stopped on a record written at the other size: %v", err)
	}
	// Six records: two written at the size the file announces, the three from
	// the #1523 window, and one after them.
	if len(recs) != 6 {
		t.Errorf("scanned %d records, want 6: the scan stops early instead of "+
			"reading past the short headers", len(recs))
	}
}

// A header neither size explains still stops the scan: recovery must not become
// tolerance, and a frame that fits neither size is damage.
func TestScanStopsOnAHeaderNeitherSizeExplains(t *testing.T) {
	u := &userMailbox{}
	_, err := u.scanMFileAt(filepath.Join("testdata", "m.tornheader"))
	if err == nil {
		t.Fatal("a torn header was accepted: the recovery reads any frame instead of the other size")
	}
	if !strings.Contains(err.Error(), "does not end in LF") {
		t.Errorf("the scan failed for another reason: %v", err)
	}
}
