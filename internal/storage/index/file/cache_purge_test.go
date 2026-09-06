package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A purge is a new generation: survivors move to a file with a new file_seq,
// the extension's reset_id follows, and stale offsets die by that fact alone.
// Payloads are distinguishable, so an offset landing on another message's
// record shows.
func TestPurgeCacheIsANewGeneration(t *testing.T) {
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 4; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "m"}); err != nil {
			t.Fatal(err)
		}
	}

	indexID, resetID, ok, err := ui.CachePairIdentity(f.ID)
	if err != nil || !ok {
		t.Fatalf("pair identity: %v %v", ok, err)
	}
	path, err := ui.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := cf.AddFields([]mailindex.CacheField{{
		Name: "probe", Type: mailindex.CacheFieldVariableSize, Decision: mailindex.CacheDecisionYes,
	}})
	if err != nil {
		t.Fatal(err)
	}
	offsets := map[uint32]uint32{}
	for uid := uint32(1); uid <= 4; uid++ {
		off, aerr := cf.AppendRecord(0, []mailindex.CacheFieldValue{
			{FieldID: fid, Data: []byte("uid-" + string(rune('0'+uid)))},
		})
		if aerr != nil {
			t.Fatal(aerr)
		}
		offsets[uid] = off
	}
	cf.Close()
	if err := ui.SetCacheOffsets(f.ID, offsets); err != nil {
		t.Fatal(err)
	}
	// Dead weight for the purge to drop.
	for _, uid := range []uint32{2, 3} {
		if err := ui.ExpungeMessage(f.ID, uid); err != nil {
			t.Fatal(err)
		}
	}

	carried, _, err := ui.PurgeCache(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if carried != 2 {
		t.Errorf("carried %d records, want the 2 still pointed at", carried)
	}

	// The old reset_id must no longer open the file.
	_, newResetID, _, err := ui.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newResetID == resetID {
		t.Fatal("reset_id did not move: old offsets would still be taken as valid")
	}
	if _, oerr := mailindex.OpenCache(path, indexID, resetID); oerr == nil {
		t.Error("the purged file still opens under the OLD reset_id")
	}

	// Each survivor reads back its own record through the new pair.
	cf2, err := mailindex.OpenCache(path, indexID, newResetID)
	if err != nil {
		t.Fatalf("purged cache does not open under the new pair: %v", err)
	}
	defer cf2.Close()
	probeID, ok := cf2.FieldID("probe")
	if !ok {
		t.Fatal("the field table did not survive the purge; ids would mean nothing")
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range msgs {
		if m.CacheOffset == 0 {
			t.Errorf("uid %d lost its offset in the purge", m.UID)
			continue
		}
		vals, rerr := cf2.ReadRecord(m.CacheOffset)
		if rerr != nil {
			t.Errorf("uid %d: %v", m.UID, rerr)
			continue
		}
		want := "uid-" + string(rune('0'+m.UID))
		if got := string(vals[probeID]); got != want {
			t.Errorf("uid %d reads %q, want %q -- an offset landed on another message's record", m.UID, got, want)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("verified %d messages, want 2", seen)
	}
}

// The file shrinks to what survived, and leaves no temp behind.
func TestPurgeCacheReclaims(t *testing.T) {
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for uid := uint32(1); uid <= n; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	indexID, resetID, _, _ := ui.CachePairIdentity(f.ID)
	path, _ := ui.CachePath(f.ID)
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := cf.AddFields([]mailindex.CacheField{{
		Name: "probe", Type: mailindex.CacheFieldVariableSize, Decision: mailindex.CacheDecisionYes,
	}})
	offsets := map[uint32]uint32{}
	blob := make([]byte, 512)
	for uid := uint32(1); uid <= n; uid++ {
		off, _ := cf.AppendRecord(0, []mailindex.CacheFieldValue{{FieldID: fid, Data: blob}})
		offsets[uid] = off
	}
	cf.Close()
	if err := ui.SetCacheOffsets(f.ID, offsets); err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= n-5; uid++ {
		if err := ui.ExpungeMessage(f.ID, uid); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, reclaimed, err := ui.PurgeCache(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("file did not shrink: %d -> %d", before.Size(), after.Size())
	}
	if reclaimed <= 0 {
		t.Errorf("reclaimed = %d, want positive", reclaimed)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), mailindex.CacheFileName+".purge")); !os.IsNotExist(err) {
		t.Error("purge temp file left behind")
	}
}

// An unreadable cache is dropped -- and the generation must move with it, or
// the stamps left in the index would apply to whatever is written at those
// offsets next (#1184).
func TestPurgeCacheAbandonsTheGenerationOfAnUnreadableFile(t *testing.T) {
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m"}); err != nil {
		t.Fatal(err)
	}
	indexID, resetID, _, err := ui.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := ui.CachePath(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	cf.Close()
	if err := ui.SetCacheOffsets(f.ID, map[uint32]uint32{1: 512}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ui.PurgeCache(f.ID); err != nil {
		t.Fatal(err)
	}
	_, newResetID, _, err := ui.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newResetID == resetID {
		t.Error("the generation did not move: stamps would apply to the next file written here")
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].CacheOffset != 0 {
		t.Errorf("stale stamp survived: %+v", msgs)
	}
}
