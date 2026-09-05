package mdbox

import (
	"bytes"
	"os"
	"path/filepath"
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

// The artefact normalised: three frames at the other size become the announced
// one, the bodies survive, and the original stays beside it.
func TestNormaliseTheFieldArtefact(t *testing.T) {
	src := os.Getenv("YARILO_MDBOX_ARTEFACT")
	if src == "" {
		t.Skip("set YARILO_MDBOX_ARTEFACT to the preserved m.14")
	}
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "m.14")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	before := framesAt(t, path)
	rewrote, err := normaliseFrames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewrote) == 0 {
		t.Fatal("the artefact was left as it was")
	}
	after := framesAt(t, path)
	t.Logf("frames before %v, after %v", before, after)
	if after[30] != 0 {
		t.Errorf("%d records still carry the other size", after[30])
	}
	if after[32] != before[30]+before[32] {
		t.Errorf("the rewrite carried %d records, want %d", after[32], before[30]+before[32])
	}
	kept, err := os.ReadFile(path + brokenSuffix)
	if err != nil {
		t.Fatalf("the original was not kept: %v", err)
	}
	if !bytes.Equal(kept, orig) {
		t.Error("the .broken copy is not the artefact's bytes")
	}
	// Every body survives: the rewrite reframes, it does not repair messages.
	u := &userMailbox{}
	recs, err := u.scanMFileAt(path)
	if err != nil {
		t.Fatalf("the normalised artefact does not scan: %v", err)
	}
	if len(recs) != 27433 {
		t.Errorf("scanned %d records after the rewrite, want 27433", len(recs))
	}
}
