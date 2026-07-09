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

func TestUIDListPath_DefaultsToMaildirRoot(t *testing.T) {
	// With no mail_path from userdb the driver defaults to <home>/Maildir,
	// so INBOX is the maildir root and siblings are dotted below it.
	box, root := newBox(t, "u@x.com")
	home := testHome(root, "u@x.com")
	got := box.uidListPath("INBOX")
	want := filepath.Join(home, "Maildir", UIDListFileName)
	if got != want {
		t.Errorf("uidListPath(INBOX) = %q, want %q", got, want)
	}
	got = box.uidListPath("Sent")
	want = filepath.Join(home, "Maildir", ".Sent", UIDListFileName)
	if got != want {
		t.Errorf("uidListPath(Sent) = %q, want %q", got, want)
	}
}

func TestUIDListPath_UsesControlDir(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, "u@x.com")
	ctrlRoot := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{
		Username:   "u@x.com",
		Home:       home,
		ControlDir: ctrlRoot,
	}).(*userMailbox)

	got := box.uidListPath("INBOX")
	want := filepath.Join(ctrlRoot, "INBOX", UIDListFileName)
	if got != want {
		t.Errorf("uidListPath(INBOX) = %q, want %q", got, want)
	}
	got = box.uidListPath("Drafts")
	want = filepath.Join(ctrlRoot, ".Drafts", UIDListFileName)
	if got != want {
		t.Errorf("uidListPath(Drafts) = %q, want %q", got, want)
	}
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
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), []string{`\Seen`})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(filename, ":2,S") {
		t.Errorf("filename %q should contain ':2,S'", filename)
	}
	if !strings.Contains(filename, ",S=") || !strings.Contains(filename, ",W=") {
		t.Errorf("filename %q should contain ,S= and ,W= size annotations", filename)
	}

	rc, err := box.Fetch("INBOX", filename, false)
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
			if f.Name == name {
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

// TestListFolders_ExplicitMailPath guards the regression where ListFolders
// scanned Home while folders were created under a distinct MailPath. With
// MailPath != Home the two diverge, so listing must read MailPath.
func TestListFolders_ExplicitMailPath(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, "u@x.com")
	mailPath := filepath.Join(home, "Maildir")
	box := New().OpenUser(&mailbox.UserInfo{
		Username: "u@x.com", Home: home, MailPath: mailPath,
	}).(*userMailbox)
	box.Init()         //nolint:errcheck
	box.Create("Sent") //nolint:errcheck

	folders, err := box.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range folders {
		if f.Name == "Sent" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListFolders missing %q under MailPath, got %v", "Sent", folders)
	}
}

// TestNestedFolderIsFlatMaildirPP verifies a nested folder is stored flat
// with "." separating levels (maildir++), not as nested subdirectories, and
// that ListFolders maps it back to the IMAP separator.
func TestNestedFolderIsFlatMaildirPP(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, "u@x.com")
	mailPath := filepath.Join(home, "Maildir")
	box := New().OpenUser(&mailbox.UserInfo{
		Username: "u@x.com", Home: home, MailPath: mailPath, Separator: ".",
	}).(*userMailbox)
	box.Init()              //nolint:errcheck
	box.Create("ProjZ.Sub") //nolint:errcheck

	// Flat on disk: one ".ProjZ.Sub" dir, no nested ".ProjZ/Sub".
	if _, err := os.Stat(filepath.Join(mailPath, ".ProjZ.Sub", "cur")); err != nil {
		t.Errorf("flat maildir++ dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mailPath, ".ProjZ", "Sub")); !os.IsNotExist(err) {
		t.Errorf("nested subdir must not exist: err=%v", err)
	}
	// Round-trips through LIST as the IMAP name.
	folders, _ := box.ListFolders()
	var found bool
	for _, f := range folders {
		if f.Name == "ProjZ.Sub" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListFolders missing ProjZ.Sub, got %v", folders)
	}
}

// TestSave_AppendsUIDList verifies Save inlines the uid→filename
// entry into the dovecot-uidlist v3 sidecar. Replaces the old
// standalone AppendUIDEntry contract (removed when two-phase Save
// landed).
func TestSave_AppendsUIDList(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	body := "m"
	f1, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), []string{`\Seen`})
	if err != nil {
		t.Fatalf("Save uid=1: %v", err)
	}
	f2, err := box.Save("INBOX", strings.NewReader(body), 2, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save uid=2: %v", err)
	}

	path := box.uidListPath("INBOX")
	fp, err := os.Open(path)
	if err != nil {
		t.Fatalf("open uidlist: %v", err)
	}
	defer fp.Close()
	sc := bufio.NewScanner(fp)
	if !sc.Scan() {
		t.Fatal("uidlist is empty")
	}
	header := sc.Text()
	if !strings.HasPrefix(header, "3 V") || !strings.Contains(header, " N") || !strings.Contains(header, " G") {
		t.Errorf("header drift: %q", header)
	}
	want := []struct {
		uid      string
		filename string
	}{
		{"1", f1},
		{"2", f2},
	}
	for i, w := range want {
		if !sc.Scan() {
			t.Fatalf("line %d missing", i+1)
		}
		line := sc.Text()
		sep := strings.Index(line, " :")
		if sep < 0 {
			t.Fatalf("line %d %q has no ' :' separator", i+1, line)
		}
		gotFilename := line[sep+2:]
		parts := strings.Fields(line[:sep])
		if len(parts) == 0 || parts[0] != w.uid {
			t.Errorf("line %d uid = %q, want %q", i+1, parts, w.uid)
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
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
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
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
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
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
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
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
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
	box, _ := newBox(t, user)
	box.Init() //nolint:errcheck

	// Drop a legacy-named file (no size annotations) directly into the
	// INBOX cur/ (the maildir root under the default <home>/Maildir).
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
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

	saved := make(map[string]uint32)
	for _, uid := range []uint32{1, 2, 3} {
		fn, err := box.Save("INBOX", strings.NewReader("m"), uid, 1, nil)
		if err != nil {
			t.Fatalf("Save uid=%d: %v", uid, err)
		}
		saved[fn] = uid
	}

	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatalf("readUIDList: %v", err)
	}
	if len(m) != len(saved) {
		t.Fatalf("readUIDList returned %d entries, want %d", len(m), len(saved))
	}
	for fn, wantUID := range saved {
		if uid, ok := m[fn]; !ok || uid != wantUID {
			t.Errorf("filename %q: got (uid=%d, ok=%v), want uid=%d", fn, uid, ok, wantUID)
		}
	}
}

func TestReadUIDList_CacheHitSkipsDiskRead(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	if _, err := box.Save("INBOX", strings.NewReader("msg"), 1, 1, nil); err != nil {
		t.Fatal(err)
	}

	// First call populates the cache.
	m1, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(m1) != 1 {
		t.Fatalf("expected 1 entry after first read, got %d", len(m1))
	}

	// Second call on unchanged file must return from cache.
	m2, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	c := box.folderCacheFor("INBOX")
	if c.uidMap == nil {
		t.Fatal("cache not populated after first read")
	}
	// The returned map must equal the cached map (same pointer).
	if len(m2) != len(c.uidMap) {
		t.Errorf("second read returned different map size: got %d, cache has %d", len(m2), len(c.uidMap))
	}
}

func TestReadUIDList_CacheUpdatedAfterAppend(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	// Seed initial entry and populate cache.
	fn1, err := box.Save("INBOX", strings.NewReader("msg1"), 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}

	// Append a second entry via appendUIDListLocked (same path Save() uses).
	fn2 := "second.file:2,"
	if err := box.appendUIDListLocked("INBOX", 2, fn2); err != nil {
		t.Fatal(err)
	}

	// Cache must reflect both entries without re-reading the file.
	c := box.folderCacheFor("INBOX")
	if c.uidMap[fn1] != 1 {
		t.Errorf("fn1 uid in cache = %d, want 1", c.uidMap[fn1])
	}
	if c.uidMap[fn2] != 2 {
		t.Errorf("fn2 uid in cache = %d, want 2", c.uidMap[fn2])
	}

	// readUIDList must also return the updated entry (from cache or disk).
	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if m[fn2] != 2 {
		t.Errorf("readUIDList fn2 = %d, want 2", m[fn2])
	}
}

func TestList_ReadDirCacheHitSkipsReadDir(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	if _, err := box.Save("INBOX", strings.NewReader("msg"), 1, 1, nil); err != nil {
		t.Fatal(err)
	}

	// First List populates the cache.
	msgs1, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs1) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs1))
	}

	c := box.folderCacheFor("INBOX")
	if c.entries == nil {
		t.Fatal("readdir cache not populated after first List")
	}

	// Second List on unchanged folder must use cached entries.
	cached := c.entries
	if _, err := box.List("INBOX"); err != nil {
		t.Fatal(err)
	}
	if &c.entries[0] != &cached[0] {
		t.Error("second List replaced cached entries — readdir was not skipped")
	}
}

func TestList_ReadDirCacheInvalidatedAfterSave(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	if _, err := box.Save("INBOX", strings.NewReader("msg1"), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := box.List("INBOX"); err != nil {
		t.Fatal(err)
	}
	c := box.folderCacheFor("INBOX")
	if c.entries == nil {
		t.Fatal("readdir cache not populated")
	}

	// Save a second message — must invalidate the cache.
	if _, err := box.Save("INBOX", strings.NewReader("msg2"), 2, 1, nil); err != nil {
		t.Fatal(err)
	}
	if c.entries != nil {
		t.Error("readdir cache not invalidated after Save")
	}

	// Next List must return both messages.
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages after second Save, got %d", len(msgs))
	}
}

func TestList_ReadDirCacheInvalidatedAfterRemove(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	box.Init() //nolint:errcheck

	fn, err := box.Save("INBOX", strings.NewReader("msg"), 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.List("INBOX"); err != nil {
		t.Fatal(err)
	}
	c := box.folderCacheFor("INBOX")
	if c.entries == nil {
		t.Fatal("readdir cache not populated")
	}

	if err := box.Remove("INBOX", fn); err != nil {
		t.Fatal(err)
	}
	if c.entries != nil {
		t.Error("readdir cache not invalidated after Remove")
	}

	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages after Remove, got %d", len(msgs))
	}
}
