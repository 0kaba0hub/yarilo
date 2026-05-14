package maildir

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := "alice@example.com"
	folder := "INBOX"

	if err := b.Init(user); err != nil {
		t.Fatal(err)
	}

	body := "From: test@example.com\r\nSubject: Test\r\n\r\nHello\r\n"
	filename, err := b.Save(user, folder, strings.NewReader(body), int64(len(body)), []string{`\Seen`})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(filename, ":2,S") {
		t.Errorf("filename %q should contain ':2,S'", filename)
	}
	if !strings.Contains(filename, ",S=") || !strings.Contains(filename, ",W=") {
		t.Errorf("filename %q should contain ,S= and ,W= size annotations", filename)
	}

	rc, err := b.Fetch(user, folder, filename)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if err := b.Remove(user, folder, filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Double remove must not error.
	if err := b.Remove(user, folder, filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}

func TestFolderExists(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	ok, err := b.FolderExists("u@x.com", "INBOX")
	if err != nil || !ok {
		t.Fatalf("INBOX should exist after Init, got ok=%v err=%v", ok, err)
	}
	ok, err = b.FolderExists("u@x.com", "NoSuchFolder")
	if err != nil || ok {
		t.Fatalf("NoSuchFolder should not exist, got ok=%v err=%v", ok, err)
	}
}

func TestCreate_Delete(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	if err := b.Create("u@x.com", "Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ := b.FolderExists("u@x.com", "Sent")
	if !ok {
		t.Fatal("Sent folder should exist after Create")
	}

	if err := b.Delete("u@x.com", "Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ = b.FolderExists("u@x.com", "Sent")
	if ok {
		t.Fatal("Sent folder should not exist after Delete")
	}
}

func TestListFolders(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com")             //nolint:errcheck
	b.Create("u@x.com", "Sent")   //nolint:errcheck
	b.Create("u@x.com", "Drafts") //nolint:errcheck

	folders, err := b.ListFolders("u@x.com")
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	if err := b.AppendUIDEntry("u@x.com", "INBOX", 1, "msg1:2,S"); err != nil {
		t.Fatalf("AppendUIDEntry: %v", err)
	}
	if err := b.AppendUIDEntry("u@x.com", "INBOX", 2, "msg2:2,"); err != nil {
		t.Fatalf("AppendUIDEntry: %v", err)
	}

	path := b.uidListPath("u@x.com", "INBOX")
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
	b, _ := New(t.TempDir())
	user := "u@x"
	b.Init(user) //nolint:errcheck

	body := "From: a@b\r\n\r\nhello\r\n"
	filename, err := b.Save(user, "INBOX", strings.NewReader(body), int64(len(body)), nil)
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
	b, _ := New(t.TempDir())
	user := "u@x"
	b.Init(user) //nolint:errcheck

	body := "From: a@b\n\nhello\n"
	filename, err := b.Save(user, "INBOX", strings.NewReader(body), int64(len(body)), nil)
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
	b, _ := New(t.TempDir())
	user := "u@x"
	b.Init(user) //nolint:errcheck

	body := "header: ok\r\nbare-lf-after\n"
	filename, err := b.Save(user, "INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, _, _ := parseSizeInfo(filename)
	if virt != phys+1 {
		t.Errorf("virt=%d, phys=%d, want virt=phys+1 (one bare LF)", virt, phys)
	}
}

func TestList_PopulatesSizesFromFilename(t *testing.T) {
	b, _ := New(t.TempDir())
	user := "u@x"
	b.Init(user) //nolint:errcheck

	body := "From: a@b\n\nhello\n"
	filename, err := b.Save(user, "INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	phys, virt, _, _ := parseSizeInfo(filename)

	msgs, err := b.List(user, "INBOX")
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
	dir := t.TempDir()
	b, _ := New(dir)
	user := "u@x"
	b.Init(user) //nolint:errcheck

	// Drop a legacy-named file (no size annotations) directly into cur/.
	// User "u@x" lives at <root>/x/u (Dovecot virtual-hosting layout).
	cur := filepath.Join(dir, "x", "u", "INBOX", "cur")
	legacy := filepath.Join(cur, "1700000000.M0P0_0.host:2,")
	body := []byte("legacy body\n")
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs, err := b.List(user, "INBOX")
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	entries := []struct {
		uid      uint32
		filename string
	}{
		{1, "aaa.bbb:2,S"},
		{2, "ccc.ddd:2,FS"},
		{3, "eee.fff:2,"},
	}
	for _, e := range entries {
		if err := b.AppendUIDEntry("u@x.com", "INBOX", e.uid, e.filename); err != nil {
			t.Fatalf("AppendUIDEntry uid=%d: %v", e.uid, err)
		}
	}

	m, err := b.readUIDList("u@x.com", "INBOX")
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
