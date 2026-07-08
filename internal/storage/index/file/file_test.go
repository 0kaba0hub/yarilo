package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/dict"
	"github.com/0kaba0hub/yarilo/pkg/dict/memory"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

const (
	testUser  = "u@x.com"
	testUser2 = "alice@x.com"
)

func testHome(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

func openIdx(root, user string) *userIndex {
	home := testHome(root, user)
	return New().OpenUser(&mailbox.UserInfo{Username: user, Home: home}).(*userHandle).ui
}

// TestIndexDirRootResolution locks the index-root resolution:
// index root is INDEX= (IndexDir), else the mail root (MailPath), else Home.
func TestIndexDirRootResolution(t *testing.T) {
	cases := []struct {
		name              string
		home, mail, index string
		wantRoot          string
	}{
		{"home only", "/h", "", "", "/h"},
		{"mail root fallback", "/h", "/h/Maildir", "", "/h/Maildir"},
		{"index overrides mail", "/h", "/h/Maildir", "/idx", "/idx"},
		{"index over home", "/h", "", "/idx", "/idx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ui := New().OpenUser(&mailbox.UserInfo{
				Username: "u@x", Home: c.home, MailPath: c.mail, IndexDir: c.index,
			}).(*userHandle).ui
			if got := ui.indexDir("INBOX"); got != c.wantRoot {
				t.Errorf("indexDir(INBOX)=%q, want root %q", got, c.wantRoot)
			}
			if got := ui.indexDir("Sent"); got != filepath.Join(c.wantRoot, ".Sent") {
				t.Errorf("indexDir(Sent)=%q, want root %q", got, c.wantRoot)
			}
		})
	}
}

// TestIndexDirMirrorsDriverLayout locks that the fileindex per-folder dir uses
// the mailbox driver's layout (mailboxes/<f> for dbox) rooted at the index root.
func TestIndexDirMirrorsDriverLayout(t *testing.T) {
	cases := []struct {
		driver, folder, want string
	}{
		{"maildir", "Sent", ".Sent"},
		{"mdbox", "INBOX", "mailboxes/INBOX"},
		{"mdbox", "Sent", "mailboxes/Sent"},
		{"sdbox", "Sent", "mailboxes/Sent/dbox-Mails"},
	}
	for _, c := range cases {
		ui := New().OpenUser(&mailbox.UserInfo{
			Username: "u@x", Home: "/h", IndexDir: "/idx", Driver: c.driver,
		}).(*userHandle).ui
		if got := ui.indexDir(c.folder); got != filepath.Join("/idx", c.want) {
			t.Errorf("driver %s indexDir(%s)=%q, want /idx/%s", c.driver, c.folder, got, c.want)
		}
	}
}

// TestDeleteFolderRemovesIndexDir locks that DeleteFolder reclaims the
// per-folder index directory so the index never outlives its mailbox,
// and that a second call on the already-gone folder is a no-op.
func TestDeleteFolderRemovesIndexDir(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("Sent", 7)
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Seen`}}) //nolint:errcheck

	fdir := b.indexDir("Sent")
	if _, err := os.Stat(fdir); err != nil {
		t.Fatalf("index dir not created: %v", err)
	}
	if err := b.DeleteFolder("Sent"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if _, err := os.Stat(fdir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("index dir survived DeleteFolder: stat err=%v", err)
	}
	// Idempotent: deleting a folder with no index dir must not error.
	if err := b.DeleteFolder("Sent"); err != nil {
		t.Errorf("second DeleteFolder should be no-op, got %v", err)
	}
}

func TestLogReplay(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 42)

	// Append 3 messages, flag-update one, expunge one.
	for i := uint32(1); i <= 3; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, Flags: []string{`\Seen`}, ModSeq: modseq}) //nolint:errcheck
	}
	b.UpdateFlags(f.ID, 2, []string{`\Seen`, `\Flagged`}, nil) //nolint:errcheck
	b.ExpungeMessage(f.ID, 3)                                  //nolint:errcheck
	b.Close()                                                  //nolint:errcheck

	// Reopen — all state must come from replaying .index.log.
	b2 := openIdx(dir, testUser)
	f2, err := b2.OpenFolder("INBOX", 42)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("after log replay: got %d messages, want 2", len(msgs))
	}
	// UID 2 must have \Flagged
	var found bool
	for _, m := range msgs {
		if m.UID == 2 {
			found = true
			hasFlagged := false
			for _, fl := range m.Flags {
				if fl == `\Flagged` {
					hasFlagged = true
				}
			}
			if !hasFlagged {
				t.Errorf("UID 2: expected \\Flagged in %v", m.Flags)
			}
		}
		if m.UID == 3 {
			t.Error("expunged UID 3 still present after log replay")
		}
	}
	if !found {
		t.Error("UID 2 missing after log replay")
	}
	b2.Close() //nolint:errcheck
}

func TestLogReplay_Keywords(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	b.AppendMessage(f.ID, &mailbox.MessageMeta{ //nolint:errcheck
		UID:      1,
		Flags:    []string{`\Seen`},
		Keywords: []string{"$Forwarded"},
		ModSeq:   modseq,
	})
	b.Close() //nolint:errcheck

	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 1)
	msgs, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs) != 1 {
		t.Fatalf("after keyword log replay: got %d messages, want 1", len(msgs))
	}
	found := false
	for _, kw := range msgs[0].Keywords {
		if kw == "$Forwarded" {
			found = true
		}
	}
	if !found {
		t.Errorf("$Forwarded missing after log replay: %v", msgs[0].Keywords)
	}
	b2.Close() //nolint:errcheck
}

func TestOpenFolder_CreateAndReopen(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser2)

	f, err := b.OpenFolder("INBOX", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if f.UIDValidity != 12345 {
		t.Errorf("UIDValidity = %d, want 12345", f.UIDValidity)
	}
	if f.NextUID != 1 {
		t.Errorf("NextUID = %d, want 1", f.NextUID)
	}
	b.Close() //nolint:errcheck

	// Reopen — must restore header from disk.
	b2 := openIdx(dir, testUser2)
	f2, err := b2.OpenFolder("INBOX", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if f2.UIDValidity != f.UIDValidity {
		t.Errorf("reopened UIDValidity = %d, want %d", f2.UIDValidity, f.UIDValidity)
	}
	b2.Close() //nolint:errcheck
}

func TestAppendAndGetMessages(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	for i := uint32(1); i <= 5; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		m := &mailbox.MessageMeta{UID: i, Flags: []string{`\Seen`}, ModSeq: modseq}
		if err := b.AppendMessage(f.ID, m); err != nil {
			t.Fatalf("AppendMessage uid=%d: %v", i, err)
		}
	}

	msgs, err := b.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5", len(msgs))
	}
	b.Close() //nolint:errcheck
}

func TestUpdateFlags(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Seen`}, ModSeq: modseq}) //nolint:errcheck

	if err := b.UpdateFlags(f.ID, 1, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
	if len(msgs) == 0 {
		t.Fatal("message not found after UpdateFlags")
	}
	hasFlag := func(flags []string, fl string) bool {
		for _, f := range flags {
			if f == fl {
				return true
			}
		}
		return false
	}
	if !hasFlag(msgs[0].Flags, `\Flagged`) {
		t.Errorf("expected \\Flagged in %v", msgs[0].Flags)
	}
	b.Close() //nolint:errcheck
}

func TestExpungeMessage(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	for i := uint32(1); i <= 3; i++ {
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, Flags: []string{`\Deleted`}}) //nolint:errcheck
	}
	if err := b.ExpungeMessage(f.ID, 2); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) != 2 {
		t.Fatalf("after expunge: got %d messages, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.UID == 2 {
			t.Error("expunged UID 2 still present")
		}
	}
	b.Close() //nolint:errcheck
}

func TestSeqSetContains(t *testing.T) {
	cases := []struct {
		s    mailbox.SeqSet
		uid  uint32
		want bool
	}{
		{mailbox.SeqSet{}, 99, true}, // empty = all
		{mailbox.SeqSet{{From: 1, To: 5}}, 3, true},
		{mailbox.SeqSet{{From: 1, To: 5}}, 6, false},
		{mailbox.SeqSet{{From: 10, To: 0}}, 999999, true}, // To=0 means *
		{mailbox.SeqSet{{From: 1, To: 3}, {From: 7, To: 9}}, 8, true},
		{mailbox.SeqSet{{From: 1, To: 3}, {From: 7, To: 9}}, 5, false},
	}
	for _, tc := range cases {
		got := seqSetContains(tc.s, tc.uid)
		if got != tc.want {
			t.Errorf("seqSetContains(%v, %d) = %v, want %v", tc.s, tc.uid, got, tc.want)
		}
	}
}

func TestKeywordsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID:      1,
		Flags:    []string{`\Seen`},
		Keywords: []string{"$Forwarded", "$Junk"},
		ModSeq:   modseq,
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	hasKW := func(kws []string, k string) bool {
		for _, kw := range kws {
			if kw == k {
				return true
			}
		}
		return false
	}
	if !hasKW(msgs[0].Keywords, "$Forwarded") {
		t.Errorf("$Forwarded not in keywords: %v", msgs[0].Keywords)
	}
	if !hasKW(msgs[0].Keywords, "$Junk") {
		t.Errorf("$Junk not in keywords: %v", msgs[0].Keywords)
	}
	b.Close() //nolint:errcheck

	// Verify keywords survive a close+reopen (disk persistence).
	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 1)
	msgs2, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs2) != 1 {
		t.Fatalf("after reopen: got %d messages, want 1", len(msgs2))
	}
	if !hasKW(msgs2[0].Keywords, "$Forwarded") {
		t.Errorf("after reopen: $Forwarded not in keywords: %v", msgs2[0].Keywords)
	}
	b2.Close() //nolint:errcheck
}

func TestKeywordsUpdateFlags(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Seen`}}) //nolint:errcheck

	if err := b.UpdateFlags(f.ID, 1, []string{`\Seen`}, []string{"$NotJunk"}); err != nil {
		t.Fatal(err)
	}

	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) == 0 {
		t.Fatal("no messages after UpdateFlags")
	}
	found := false
	for _, kw := range msgs[0].Keywords {
		if kw == "$NotJunk" {
			found = true
		}
	}
	if !found {
		t.Errorf("$NotJunk not in keywords after UpdateFlags: %v", msgs[0].Keywords)
	}
	b.Close() //nolint:errcheck
}

func TestFlagConversion(t *testing.T) {
	flags := []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}
	idx := imapFlagsToIndex(flags)
	back := indexFlagsToIMAP(idx)

	has := func(sl []string, f string) bool {
		for _, s := range sl {
			if s == f {
				return true
			}
		}
		return false
	}
	for _, f := range flags {
		if !has(back, f) {
			t.Errorf("flag %q lost in conversion (index byte: %08b)", f, idx)
		}
	}
}

// TestNextModSeqIsMonotonic regresses a pre-fix bug where NextModSeq
// bumped the in-memory modseq but never persisted to the on-disk
// header. Because AllocateAppend's rereadHeaderLocked reset the
// in-memory value to the stale disk value, successive NextModSeq
// calls all returned the same number → modseqs were not unique
// across messages, which broke CONDSTORE / QRESYNC client caches
// under load. The fix persists every modseq bump and the reread
// keeps the higher of (disk, in-memory).
func TestNextModSeqIsMonotonic(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}

	// Sequence: NextModSeq → AllocateAppend → NextModSeq → AllocateAppend.
	// Pre-fix, both messages ended up with rec.modseq == 2 and the on-disk
	// header.modseq stayed at 1. Post-fix the values must strictly
	// increase.
	var modseqs [4]uint64
	var uids [2]uint32
	for i := 0; i < 2; i++ {
		ms, err := b.NextModSeq(f.ID)
		if err != nil {
			t.Fatalf("NextModSeq #%d: %v", i, err)
		}
		modseqs[i*2] = ms
		uid, err := b.AllocateUID(f.ID)
		if err != nil {
			t.Fatalf("AllocateUID #%d: %v", i, err)
		}
		if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, ModSeq: ms, Flags: []string{}}); err != nil {
			t.Fatalf("AppendMessage #%d: %v", i, err)
		}
		uids[i] = uid

		// Re-read the message's persisted modseq via GetMessages so the
		// test exercises the full write-then-read path.
		msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{{From: uid, To: uid}})
		if len(msgs) != 1 {
			t.Fatalf("GetMessages returned %d msgs, want 1", len(msgs))
		}
		modseqs[i*2+1] = msgs[0].ModSeq
	}

	// First NextModSeq must return strictly less than second NextModSeq.
	if modseqs[0] >= modseqs[2] {
		t.Errorf("NextModSeq not monotonic: first=%d second=%d (should grow)", modseqs[0], modseqs[2])
	}
	// Each message's persisted modseq must equal the NextModSeq value
	// the caller passed at AllocateAppend time.
	if modseqs[1] != modseqs[0] {
		t.Errorf("msg1 persisted modseq %d != allocated %d", modseqs[1], modseqs[0])
	}
	if modseqs[3] != modseqs[2] {
		t.Errorf("msg2 persisted modseq %d != allocated %d", modseqs[3], modseqs[2])
	}

	// Folder header high-watermark must reflect the latest allocation
	// (visible across process restarts).
	reopened, err := b.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("re-open folder: %v", err)
	}
	if reopened.HighestModSeq < modseqs[2] {
		t.Errorf("header HighestModSeq %d < last allocation %d (persistence regressed)",
			reopened.HighestModSeq, modseqs[2])
	}
}

// TestUpdateFlagsPersistsModSeqBump verifies the second of the three
// modseq bump sites — UpdateFlags — also persists. Pre-fix, STORE
// from one session would bump in-memory only and the next reread by
// another process would revert.
func TestUpdateFlagsPersistsModSeqBump(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	ms, _ := b.NextModSeq(f.ID)
	uid, _ := b.AllocateUID(f.ID)
	_ = b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, ModSeq: ms, Flags: []string{}})

	if err := b.UpdateFlags(f.ID, uid, []string{`\Seen`}, nil); err != nil {
		t.Fatalf("UpdateFlags: %v", err)
	}

	// Re-open folder → header modseq must reflect the UpdateFlags bump,
	// not just the original allocation.
	reopened, _ := b.OpenFolder("INBOX", 1)
	if reopened.HighestModSeq <= ms {
		t.Errorf("UpdateFlags did not persist modseq bump: header=%d, allocation=%d",
			reopened.HighestModSeq, ms)
	}
}

// TestExpungePersistsModSeqBump verifies the third bump site —
// ExpungeMessage — persists. Without the fix, QRESYNC VANISHED
// queries from a follower session would not see the expunge with a
// modseq strictly greater than the last-known watermark.
func TestExpungePersistsModSeqBump(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	ms, _ := b.NextModSeq(f.ID)
	uid, _ := b.AllocateUID(f.ID)
	_ = b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, ModSeq: ms, Flags: []string{}})
	beforeHeader, _ := b.OpenFolder("INBOX", 1)

	if err := b.ExpungeMessage(f.ID, uid); err != nil {
		t.Fatalf("ExpungeMessage: %v", err)
	}

	reopened, _ := b.OpenFolder("INBOX", 1)
	if reopened.HighestModSeq <= beforeHeader.HighestModSeq {
		t.Errorf("ExpungeMessage did not persist modseq bump: header=%d, pre-expunge=%d",
			reopened.HighestModSeq, beforeHeader.HighestModSeq)
	}
}

func newMemDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("memory dict: %v", err)
	}
	return d
}

func TestSize_RoundtripAndReopen(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "a", Size: 1234}) //nolint:errcheck
	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 2, Filename: "b", Size: 5678}) //nolint:errcheck
	b.Close()                                                                      //nolint:errcheck

	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 1)
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	for _, m := range msgs {
		var want uint32
		switch m.UID {
		case 1:
			want = 1234
		case 2:
			want = 5678
		}
		if m.Size != want {
			t.Errorf("UID %d: Size = %d, want %d", m.UID, m.Size, want)
		}
	}
	b2.Close() //nolint:errcheck
}

func TestQuota_IgnoreFolderSkipsCounter(t *testing.T) {
	d := newMemDict(t)
	dir := t.TempDir()
	home := testHome(dir, testUser)

	lim := quota.Limits{}
	lim.PerFolder = map[string]quota.FolderRule{
		"Spam": {Ignore: true},
	}
	b := New(WithQuotaCounter(func(u *mailbox.UserInfo) (*quota.Counter, quota.Limits) {
		return quota.NewCounter(d, u.Username), lim
	})).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui

	inbox, _ := b.OpenFolder("INBOX", 1)
	spam, _ := b.OpenFolder("Spam", 1)

	b.AppendMessage(inbox.ID, &mailbox.MessageMeta{UID: 1, Size: 1000}) //nolint:errcheck
	b.AppendMessage(spam.ID, &mailbox.MessageMeta{UID: 1, Size: 9999})  //nolint:errcheck — must NOT update counter

	ctr := quota.NewCounter(d, testUser)
	u, err := ctr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 1000 {
		t.Errorf("after Spam append (ignored): StorageBytes = %d, want 1000", u.StorageBytes)
	}
	if u.Messages != 1 {
		t.Errorf("after Spam append (ignored): Messages = %d, want 1", u.Messages)
	}
	b.Close() //nolint:errcheck
}

func TestQuota_AdditiveFolder(t *testing.T) {
	d := newMemDict(t)
	dir := t.TempDir()
	home := testHome(dir, testUser)

	const G = int64(1024 * 1024 * 1024)
	lim := quota.ParseRules([]string{"*:storage=5G", "Trash:storage=+1G"})
	b := New(WithQuotaCounter(func(u *mailbox.UserInfo) (*quota.Counter, quota.Limits) {
		return quota.NewCounter(d, u.Username), lim
	})).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui

	trash, _ := b.OpenFolder("Trash", 1)
	b.AppendMessage(trash.ID, &mailbox.MessageMeta{UID: 1, Size: uint32(500)}) //nolint:errcheck

	// Trash is NOT ignored — counter still increments.
	ctr := quota.NewCounter(d, testUser)
	u, err := ctr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 500 {
		t.Errorf("Trash counter: StorageBytes = %d, want 500", u.StorageBytes)
	}

	// Effective limit for Trash is 6G (5G + 1G).
	effLim, ignore := lim.EffectiveLimits("Trash")
	if ignore {
		t.Error("Trash should not be ignored")
	}
	if effLim.StorageBytes != 6*G {
		t.Errorf("Trash effective limit = %d, want 6G", effLim.StorageBytes)
	}
	b.Close() //nolint:errcheck
}

func TestQuota_CounterTracking(t *testing.T) {
	d := newMemDict(t)
	dir := t.TempDir()

	home := testHome(dir, testUser)
	b := New(WithQuotaCounter(func(u *mailbox.UserInfo) (*quota.Counter, quota.Limits) {
		return quota.NewCounter(d, u.Username), quota.Limits{}
	})).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui

	f, _ := b.OpenFolder("INBOX", 1)

	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 1000}) //nolint:errcheck
	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 2, Size: 2000}) //nolint:errcheck

	ctr := quota.NewCounter(d, testUser)
	u, err := ctr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 3000 {
		t.Errorf("after 2 appends: StorageBytes = %d, want 3000", u.StorageBytes)
	}
	if u.Messages != 2 {
		t.Errorf("after 2 appends: Messages = %d, want 2", u.Messages)
	}

	b.ExpungeMessage(f.ID, 1) //nolint:errcheck

	u, err = ctr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.StorageBytes != 2000 {
		t.Errorf("after expunge uid=1: StorageBytes = %d, want 2000", u.StorageBytes)
	}
	if u.Messages != 1 {
		t.Errorf("after expunge uid=1: Messages = %d, want 1", u.Messages)
	}
	b.Close() //nolint:errcheck
}

// TestSaveFolderDoesNotCorruptMessagesCount covers the bug where SaveFolder
// was writing a stale session counter into the on-disk header, causing
// MessagesCount to drift from the actual record count. After the fix, flush
// always recounts from fs.file.Records so any drift is healed on the next write.
func TestSaveFolderDoesNotCorruptMessagesCount(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	// Append 5 messages.
	for i := uint32(1); i <= 5; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, ModSeq: modseq}) //nolint:errcheck
	}

	// Simulate what the IMAP MOVE handler used to do: set the Folder counter
	// to a stale/wrong value and call SaveFolder. Before the fix this would
	// overwrite MessagesCount on disk with the bogus value.
	staleFolder := &mailbox.Folder{ID: f.ID, Name: "INBOX", Messages: 0}
	if err := b.SaveFolder(staleFolder); err != nil {
		t.Fatalf("SaveFolder: %v", err)
	}

	// Reopen from disk — records must all survive (counter must not corrupt data).
	b.Close() //nolint:errcheck
	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 1)
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 records, got %d", len(msgs))
	}
	// MessagesCount in the header must equal actual record count.
	if b2.open[f2.ID].file.Header.MessagesCount != 5 {
		t.Errorf("MessagesCount = %d after SaveFolder with stale counter, want 5",
			b2.open[f2.ID].file.Header.MessagesCount)
	}
	b2.Close() //nolint:errcheck
}

// TestApplyLogRecountsAfterCorruptedHeaderUpdate verifies that applyLog heals
// a MessagesCount that was corrupted in a TxTypeHeaderUpdate by recounting
// from the actual record list rather than trusting the on-disk header counter.
func TestApplyLogRecountsAfterCorruptedHeaderUpdate(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 1)

	// Append 3 messages and expunge 1.
	for i := uint32(1); i <= 3; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, ModSeq: modseq}) //nolint:errcheck
	}
	b.ExpungeMessage(f.ID, 2) //nolint:errcheck

	// Compact base file, then reopen.
	b.OptimizeIndex(f.ID) //nolint:errcheck
	b.Close()             //nolint:errcheck

	// Re-append to produce a non-empty log.
	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 1)
	modseq, _ := b2.NextModSeq(f2.ID)
	b2.AppendMessage(f2.ID, &mailbox.MessageMeta{UID: 4, ModSeq: modseq}) //nolint:errcheck
	b2.Close()                                                            //nolint:errcheck

	// Third open: MessagesCount must equal actual record count (2 surviving + 1 new = 3).
	b3 := openIdx(dir, testUser)
	f3, _ := b3.OpenFolder("INBOX", 1)
	msgs, _ := b3.GetMessages(f3.ID, mailbox.SeqSet{})
	if len(msgs) != 3 {
		t.Errorf("GetMessages returned %d records, want 3", len(msgs))
	}
	if b3.open[f3.ID].file.Header.MessagesCount != 3 {
		t.Errorf("MessagesCount = %d after log replay, want 3",
			b3.open[f3.ID].file.Header.MessagesCount)
	}
	b3.Close() //nolint:errcheck
}
