package mdbox

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
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
	if len(rewrote) == 0 {
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
	if again, err := normaliseFrames(path); err != nil || len(again) != 0 {
		t.Errorf("a second pass rewrote the file again (%d records, %v)", len(again), err)
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
	if len(rewrote) == 0 {
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

// A message read through the map is the same after the rewrite: re-framing
// shifts what follows, and the map holds byte offsets (#1687).
func TestAFetchThroughTheMapSurvivesTheRewrite(t *testing.T) {
	_, u := healTestUser(t)
	var names []string
	for i, body := range []string{"first body\r\n", "second body\r\n", "third body\r\n"} {
		name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, 0, nil, [16]byte{byte(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	before := fetchBody(t, u, names[2])

	// Re-frame the first record and move the map with it: that is what a store
	// written by that build looks like, and a fetch works until the rewrite.
	path := u.mfilePath(1)
	delta := reframeFirstRecord(t, path)
	shiftMapAfterFirst(t, u, delta)
	if got := fetchBody(t, u, names[2]); !bytes.Equal(got, before) {
		t.Fatalf("the fixture is not consistent: a fetch already returns %q", got)
	}

	if _, err := u.normaliseStorageFrames(); err != nil {
		t.Fatal(err)
	}
	if got := fetchBody(t, u, names[2]); !bytes.Equal(got, before) {
		t.Errorf("after the rewrite a fetch by map_uid returned %q, want %q: the map still "+
			"points at the pre-rewrite offsets", got, before)
	}
}

func fetchBody(t *testing.T, u *userMailbox, name string) []byte {
	t.Helper()
	rc, err := u.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch %s: %v", name, err)
	}
	defer rc.Close() //nolint:errcheck
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// reframeFirstRecord writes the first record's header at the size the file does
// not announce, which is what a build inside the #1523 window did.
func reframeFirstRecord(t *testing.T, path string) int {
	t.Helper()
	d, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.IndexByte(d, '\n') + 1
	lf := bytes.IndexByte(d[start:], '\n') + start
	out := append([]byte{}, d[:lf]...)
	delta := 2
	switch lf - start + 1 {
	case messageHeaderSize:
		out = append(out, ' ', ' ') // the legacy frame in a file announcing ours
	default:
		out = out[:len(out)-2] // ours in a file announcing the legacy frame
		delta = -2
	}
	out = append(out, '\n')
	out = append(out, d[lf+1:]...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return delta
}

// shiftMapAfterFirst moves every record but the first by delta, so the map
// agrees with the tampered file the way it would on a store that build wrote.
func shiftMapAfterFirst(t *testing.T, u *userMailbox, delta int) {
	t.Helper()
	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := m.RecordsInFile(1)
	if err != nil {
		t.Fatal(err)
	}
	var moved []mdboxmap.MovedRecord
	for _, e := range entries {
		if e.Offset == 0 {
			continue
		}
		moved = append(moved, mdboxmap.MovedRecord{
			UID: e.UID, FileID: 1, Offset: uint32(int(e.Offset) + delta), Size: e.Size, GUID: e.GUID,
		})
	}
	if err := m.AppendMove(moved, nil); err != nil {
		t.Fatal(err)
	}
}
