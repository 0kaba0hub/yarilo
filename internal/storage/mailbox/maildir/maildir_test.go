package maildir

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func testHome(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

func newBox(t *testing.T, user string) (*userMailbox, string) {
	t.Helper()
	root := t.TempDir()
	home := testHome(root, user)
	return New().OpenUser(&mailbox.UserInfo{Username: user, Home: home}).(*userMailbox), root
}

var encodeFlagsTests = []struct {
	flags []string
	want  string
}{
	{nil, ""},
	{[]string{`\Seen`}, "S"},
	{[]string{`\Answered`, `\Seen`}, "RS"},
	{[]string{`\Draft`, `\Flagged`, `\Deleted`, `\Seen`, `\Answered`}, "DFRST"},
	{[]string{`\Deleted`}, "T"},
}

func TestEncodeFlags(t *testing.T) {
	for _, tc := range encodeFlagsTests {
		got := encodeFlags(tc.flags)
		if got != tc.want {
			t.Errorf("encodeFlags(%v) = %q, want %q", tc.flags, got, tc.want)
		}
	}
}

func TestDecodeFlags(t *testing.T) {
	cases := []struct {
		filename string
		flags    []string
	}{
		{"1234567890.M123P456.host:2,S", []string{`\Seen`}},
		{"1234567890.M123P456.host:2,RST", []string{`\Answered`, `\Seen`, `\Deleted`}},
		{"1234567890.M123P456.host:2,", nil},
		{"1234567890.M123P456.host", nil},
		{"1234567890.M123P456.host:2,DFRST", []string{`\Draft`, `\Flagged`, `\Answered`, `\Seen`, `\Deleted`}},
	}
	for _, tc := range cases {
		got, _ := decodeFlags(tc.filename)
		if len(got) != len(tc.flags) {
			t.Errorf("decodeFlags(%q) flags = %v, want %v", tc.filename, got, tc.flags)
			continue
		}
		for i, f := range got {
			if f != tc.flags[i] {
				t.Errorf("decodeFlags(%q)[%d] = %q, want %q", tc.filename, i, f, tc.flags[i])
			}
		}
	}
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	flags := []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}
	encoded := encodeFlags(flags)
	filename := "msg:2," + encoded
	decoded, _ := decodeFlags(filename)
	if len(decoded) != len(flags) {
		t.Fatalf("roundtrip: got %v, want %v", decoded, flags)
	}
}

func TestSave_Fetch_Remove(t *testing.T) {
	user := "alice@example.com"
	box, _ := newBox(t, user)

	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	body := "From: test@example.com\r\nSubject: Test\r\n\r\nHello\r\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), []string{`\Seen`})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(filename, ":2,S") {
		t.Errorf("filename %q should contain ':2,S'", filename)
	}
	if !strings.Contains(filename, ",S=") || !strings.Contains(filename, ",W=") {
		t.Errorf("filename %q should contain ,S= and ,W= size annotations", filename)
	}

	rc, err := box.Fetch("INBOX", filename)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Double remove must not error.
	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}

func TestFolderExists(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	ok, err := box.FolderExists("INBOX")
	if err != nil || !ok {
		t.Fatalf("INBOX should exist after Init, got ok=%v err=%v", ok, err)
	}
	ok, err = box.FolderExists("NoSuchFolder")
	if err != nil || ok {
		t.Fatalf("NoSuchFolder should not exist, got ok=%v err=%v", ok, err)
	}
}

func TestCreate_Delete(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	if err := box.Create("Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ := box.FolderExists("Sent")
	if !ok {
		t.Fatal("Sent folder should exist after Create")
	}

	if err := box.Delete("Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ = box.FolderExists("Sent")
	if ok {
		t.Fatal("Sent folder should not exist after Delete")
	}
}

func TestListFolders(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init()           //nolint:errcheck
	box.Create("Sent")   //nolint:errcheck
	box.Create("Drafts") //nolint:errcheck

	folders, err := box.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	has := func(name string) bool {
		for _, f := range folders {
			if f == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts"} {
		if !has(want) {
			t.Errorf("ListFolders missing %q, got %v", want, folders)
		}
	}
}

func TestAppendUIDEntry_HeaderFormat(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	if err := box.AppendUIDEntry("INBOX", 1, "msg1:2,S"); err != nil {
		t.Fatalf("AppendUIDEntry: %v", err)
	}
	if err := box.AppendUIDEntry("INBOX", 2, "msg2:2,"); err != nil {
		t.Fatalf("AppendUIDEntry: %v", err)
	}

	path := box.uidListPath("INBOX")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open uidlist: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)

	// First line must be v3 header starting with "3 V"
	if !sc.Scan() {
		t.Fatal("uidlist is empty")
	}
	header := sc.Text()
	if !strings.HasPrefix(header, "3 V") {
		t.Errorf("header line %q does not start with '3 V'", header)
	}
	if !strings.Contains(header, " N") {
		t.Errorf("header line %q missing N<nextuid>", header)
	}
	if !strings.Contains(header, " G") {
		t.Errorf("header line %q missing G<guid>", header)
	}

	// Remaining lines: "uid :filename"
	want := []struct {
		uid      string
		filename string
	}{
		{"1", "msg1:2,S"},
		{"2", "msg2:2,"},
	}
	for i, w := range want {
		if !sc.Scan() {
			t.Fatalf("line %d missing", i+1)
		}
		line := sc.Text()
		// format: "uid :filename" — separator is " :" (space+colon)
		sep := strings.Index(line, " :")
		if sep < 0 {
			t.Fatalf("line %d %q has no ' :' separator", i+1, line)
		}
		gotFilename := line[sep+2:]
		parts := strings.Fields(line[:sep])
		if len(parts) == 0 {
			t.Fatalf("line %d %q has no uid field", i+1, line)
		}
		if parts[0] != w.uid {
			t.Errorf("line %d uid = %q, want %q", i+1, parts[0], w.uid)
		}
		if gotFilename != w.filename {
			t.Errorf("line %d filename = %q, want %q", i+1, gotFilename, w.filename)
		}
	}
}

// ---- VSize / size annotation tests -----------------------------------------

func TestParseSizeInfo(t *testing.T) {
	cases := []struct {
		name             string
		input            string
		wantPhys         uint32
		wantVirt         uint32
		hasPhys, hasVirt bool
	}{
		{"both sizes", "1234.M.host,S=100,W=110:2,S", 100, 110, true, true},
		{"only phys", "1234.M.host,S=100:2,S", 100, 0, true, false},
		{"only virt", "1234.M.host,W=110:2,S", 0, 110, false, true},
		{"no sizes", "1234.M.host:2,S", 0, 0, false, false},
		{"no flags suffix", "1234.M.host,S=200,W=210", 200, 210, true, true},
		{"bad number ignored", "1234.M.host,S=abc,W=42:2,", 0, 42, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, v, hp, hv := parseSizeInfo(tc.input)
			if p != tc.wantPhys || v != tc.wantVirt || hp != tc.hasPhys || hv != tc.hasVirt {
				t.Errorf("parseSizeInfo(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
					tc.input, p, v, hp, hv, tc.wantPhys, tc.wantVirt, tc.hasPhys, tc.hasVirt)
			}
		})
	}
}

func TestSave_VSize_PureCRLF(t *testing.T) {
	// CRLF input: virtual size equals physical (no normalisation needed).
	user := "u@x"
	box, _ := newBox(t, user)
	box.Init() //nolint:errcheck

	body := "From: a@b\r\n\r\nhello\r\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, hasPhys, hasVirt := parseSizeInfo(filename)
	if !hasPhys || !hasVirt {
		t.Fatalf("filename missing size annotations: %q", filename)
	}
	if int(phys) != len(body) {
		t.Errorf("phys=%d, want %d", phys, len(body))
	}
	if virt != phys {
		t.Errorf("virt=%d, phys=%d — pure CRLF input should have virt==phys", virt, phys)
	}
}

func TestSave_VSize_PureLF(t *testing.T) {
	// LF-only input (e.g. imported from Unix mbox): each LF becomes CRLF on
	// the wire, so virt > phys by exactly the LF count.
	user := "u@x"
	box, _ := newBox(t, user)
	box.Init() //nolint:errcheck

	body := "From: a@b\n\nhello\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, _, _ := parseSizeInfo(filename)
	if int(phys) != len(body) {
		t.Errorf("phys=%d, want %d", phys, len(body))
	}
	wantVirt := uint32(len(body)) + uint32(strings.Count(body, "\n"))
	if virt != wantVirt {
		t.Errorf("virt=%d, want %d (= %d + %d LF count)", virt, wantVirt, len(body), strings.Count(body, "\n"))
	}
}

func TestSave_VSize_MixedLineEndings(t *testing.T) {
	// One CRLF line plus one bare LF line: only the bare LF adds a byte.
	user := "u@x"
	box, _ := newBox(t, user)
	box.Init() //nolint:errcheck

	body := "header: ok\r\nbare-lf-after\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, _, _ := parseSizeInfo(filename)
	if virt != phys+1 {
		t.Errorf("virt=%d, phys=%d, want virt=phys+1 (one bare LF)", virt, phys)
	}
}

func TestList_PopulatesSizesFromFilename(t *testing.T) {
	user := "u@x"
	box, _ := newBox(t, user)
	box.Init() //nolint:errcheck

	body := "From: a@b\n\nhello\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, _, _ := parseSizeInfo(filename)

	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Size != phys {
		t.Errorf("Size=%d, want %d (from filename)", msgs[0].Size, phys)
	}
	if msgs[0].VSize != virt {
		t.Errorf("VSize=%d, want %d (from filename)", msgs[0].VSize, virt)
	}
}

func TestList_LegacyFilename_FallsBackToStat(t *testing.T) {
	// Legacy files without ,S= must still produce a non-zero Size by stat().
	user := "u@x"
	box, root := newBox(t, user)
	box.Init() //nolint:errcheck

	// Drop a legacy-named file (no size annotations) directly into cur/.
	// User "u@x" lives at <root>/x/u (Dovecot virtual-hosting layout).
	cur := filepath.Join(root, "x", "u", "INBOX", "cur")
	legacy := filepath.Join(cur, "1700000000.M0P0_0.host:2,")
	body := []byte("legacy body\n")
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Size != uint32(len(body)) {
		t.Errorf("Size=%d, want %d (stat fallback)", msgs[0].Size, len(body))
	}
	if msgs[0].VSize != 0 {
		t.Errorf("VSize=%d, want 0 (no W= for legacy file)", msgs[0].VSize)
	}
}

func TestUIDListRoundtrip(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	entries := []struct {
		uid      uint32
		filename string
	}{
		{1, "aaa.bbb:2,S"},
		{2, "ccc.ddd:2,FS"},
		{3, "eee.fff:2,"},
	}
	for _, e := range entries {
		if err := box.AppendUIDEntry("INBOX", e.uid, e.filename); err != nil {
			t.Fatalf("AppendUIDEntry uid=%d: %v", e.uid, err)
		}
	}

	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatalf("readUIDList: %v", err)
	}
	if len(m) != len(entries) {
		t.Fatalf("readUIDList returned %d entries, want %d", len(m), len(entries))
	}
	for _, e := range entries {
		uid, ok := m[e.filename]
		if !ok {
			t.Errorf("filename %q not found in uidlist", e.filename)
			continue
		}
		if uid != e.uid {
			t.Errorf("filename %q: uid = %d, want %d", e.filename, uid, e.uid)
		}
	}
}
