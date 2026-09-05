package mdbox

import (
	"os"
	"testing"
)

// The artefact from the field: 27433 records, three of them written at the
// other size. Skipped when the file is not there; it is not committed.
func TestScanTheFieldArtefact(t *testing.T) {
	path := os.Getenv("YARILO_MDBOX_ARTEFACT")
	if path == "" {
		t.Skip("set YARILO_MDBOX_ARTEFACT to the preserved m.14")
	}
	u := &userMailbox{}
	recs, err := u.scanMFileAt(path)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	t.Logf("scanned %d records", len(recs))
	if len(recs) != 27433 {
		t.Errorf("scanned %d records, want 27433", len(recs))
	}
}
