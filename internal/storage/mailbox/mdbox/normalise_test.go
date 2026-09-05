package mdbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// framesAt reports, per header size, how many records the file carries.
func framesAt(t *testing.T, path string) map[int]int {
	t.Helper()
	d, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]int{}
	pos := bytes.IndexByte(d, '\n') + 1
	for pos < len(d) && d[pos] == magicPreByte0 {
		lf := bytes.IndexByte(d[pos:], '\n')
		if lf < 0 {
			break
		}
		hdr := lf + 1
		out[hdr]++
		size, err := parseHeaderSize(d[pos : pos+hdr])
		if err != nil {
			break
		}
		body := pos + hdr + int(size)
		end := bytes.Index(d[body:], []byte("\n\n"))
		if end < 0 {
			break
		}
		pos = body + end + 2
	}
	return out
}

// A rebuild rewrites a record framed at the size the file does not announce: the
// reference has no notion of that size and refuses the file (#1687).
func TestNormalisationRewritesTheOtherSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.14")
	src, err := os.ReadFile(filepath.Join("testdata", "m.shortheader"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	before := framesAt(t, path)
	if before[30] != 3 || before[32] != 3 {
		t.Fatalf("the fixture is not what this asserts on: %v", before)
	}

	rewrote, err := normaliseFrames(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rewrote {
		t.Fatal("the file was left as it was: the frames the reference refuses are still there")
	}
	after := framesAt(t, path)
	if after[30] != 0 || after[32] != 6 {
		t.Errorf("after the rewrite the frames are %v, want six at the announced 32", after)
	}

	// The bodies survive byte for byte: this rewrites frames, it does not repair
	// messages.
	if !bytes.Contains(mustRead(t, path), []byte("Delivered-To:")) {
		t.Error("the rewritten file lost the message bodies")
	}
	broken := path + brokenSuffix
	orig, err := os.ReadFile(broken)
	if err != nil {
		t.Fatalf("the original was not kept beside it: %v", err)
	}
	if !bytes.Equal(orig, src) {
		t.Error("the .broken copy is not the original bytes")
	}
	// Idempotent: a second rebuild finds nothing to do.
	if again, err := normaliseFrames(path); err != nil || again {
		t.Errorf("a second pass rewrote the file again (%v, %v)", again, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	d, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A frame no size explains stops the copy where it is, as the reference does:
// what was read is written, the rest stays in the .broken copy.
func TestNormalisationStopsAtAFrameNeitherSizeExplains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.15")
	src, err := os.ReadFile(filepath.Join("testdata", "m.shortthentorn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	rewrote, err := normaliseFrames(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rewrote {
		t.Fatal("the three other-size records before the tear were left as they were")
	}
	after := framesAt(t, path)
	if after[30] != 0 {
		t.Errorf("frames after the rewrite: %v, want none at the other size", after)
	}
	if after[32] != 5 {
		t.Errorf("the rewrite carried %d records, want the five before the torn one: %v",
			after[32], after)
	}
	orig, err := os.ReadFile(path + brokenSuffix)
	if err != nil {
		t.Fatalf("the original was not kept: %v", err)
	}
	if !bytes.Equal(orig, src) {
		t.Error("the .broken copy is not the original bytes, so the torn record is lost")
	}
}
