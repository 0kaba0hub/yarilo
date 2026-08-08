package file

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The cache extension round-trips through the index: stamped offsets come
// back on GetMessages, unstamped messages read zero, and the pair identity
// (indexid + reset_id) is what a cache file must match (#1030).
func TestCacheOffsetRoundTrip(t *testing.T) {
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: t.TempDir()}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 3; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}

	if err := ui.SetCacheOffsets(f.ID, map[uint32]uint32{1: 128, 3: 256}); err != nil {
		t.Fatal(err)
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]uint32{1: 128, 2: 0, 3: 256}
	for _, m := range msgs {
		if m.CacheOffset != want[m.UID] {
			t.Errorf("uid %d: CacheOffset = %d, want %d", m.UID, m.CacheOffset, want[m.UID])
		}
	}

	// Overwrite with a newer offset (a record appended to the chain moves
	// the head); zero must not clobber.
	if err := ui.SetCacheOffsets(f.ID, map[uint32]uint32{1: 512, 3: 0}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = ui.GetMessages(f.ID, nil)
	want = map[uint32]uint32{1: 512, 2: 0, 3: 256}
	for _, m := range msgs {
		if m.CacheOffset != want[m.UID] {
			t.Errorf("after overwrite uid %d: CacheOffset = %d, want %d", m.UID, m.CacheOffset, want[m.UID])
		}
	}

	indexID, resetID, ok, err := ui.CachePairIdentity(f.ID)
	if err != nil || !ok || indexID == 0 || resetID == 0 {
		t.Errorf("CachePairIdentity = (%d, %d, %v, %v)", indexID, resetID, ok, err)
	}

	// An expunged message takes its offset with it: the index owns the
	// record, and the offset lives in the record.
	if err := ui.ExpungeMessage(f.ID, 1); err != nil {
		t.Fatal(err)
	}
	msgs, _ = ui.GetMessages(f.ID, nil)
	for _, m := range msgs {
		if m.UID == 1 {
			t.Fatal("uid 1 not expunged")
		}
	}
}

// The append path never persists a cache offset, whatever the meta carries:
// an offset is valid only inside its own (indexid, file_seq) pair, and every
// folder has its own (#1178). Call sites guard this too, but each new one has
// to re-earn it; this holds for all of them. SetCacheOffsets stays the only
// writer, so a rebuild that wants to keep offsets asks by name.
func TestAppendNeverPersistsACacheOffset(t *testing.T) {
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: t.TempDir()}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	// As a naive copy of the source meta would carry it.
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, CacheOffset: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := ui.AllocateAndAppend(f.ID, &mailbox.MessageMeta{CacheOffset: 8192}); err != nil {
		t.Fatal(err)
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.CacheOffset != 0 {
			t.Errorf("uid %d persisted cache offset %d from the meta; an offset from another "+
				"pair resolves to a valid-looking record for a different message", m.UID, m.CacheOffset)
		}
	}
	// Stamping still works: the guard is not "offsets are never stored".
	if err := ui.SetCacheOffsets(f.ID, map[uint32]uint32{1: 512}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = ui.GetMessages(f.ID, nil)
	for _, m := range msgs {
		if m.UID == 1 && m.CacheOffset != 512 {
			t.Errorf("explicit stamp lost: uid 1 offset = %d, want 512", m.CacheOffset)
		}
	}
}

// A folder whose index predates the extension must be able to gain one: that
// is every folder in an upgraded deployment. Without it the lazy add is
// unreachable -- only a stamping write adds the extension, and stamping needs
// the pair the missing extension prevents opening (#1184).
func TestEnsureCacheExtensionOnAnIndexThatPredatesIt(t *testing.T) {
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: t.TempDir()}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1}); err != nil {
		t.Fatal(err)
	}

	// Rewrite the index the way a version before the extension wrote it.
	if err := ui.withFolder(f.ID, func(fs *folderState) error {
		kept := fs.file.Extensions[:0]
		for _, ext := range fs.file.Extensions {
			if ext.Name != extNameCache {
				kept = append(kept, ext)
			}
		}
		for _, rec := range fs.file.Records {
			delete(rec.Ext, extNameCache)
		}
		// Recompute the layout the way addExtension does, so the header's
		// record size matches the shortened set.
		layout, lerr := mailindex.ComputeRecordLayout(kept)
		if lerr != nil {
			return lerr
		}
		extBytes, eerr := mailindex.EncodeExtHeaders(layout.Extensions)
		if eerr != nil {
			return eerr
		}
		fs.file.Extensions = layout.Extensions
		fs.file.Layout = layout
		fs.file.Header.RecordSize = layout.RecordSize
		fs.file.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
		return fs.flush(true)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := ui.CachePairIdentity(f.ID); ok || err != nil {
		t.Fatalf("precondition: extension still present (ok=%v err=%v)", ok, err)
	}

	indexID, resetID, err := ui.EnsureCacheExtension(f.ID)
	if err != nil || indexID == 0 || resetID == 0 {
		t.Fatalf("EnsureCacheExtension = (%d, %d, %v)", indexID, resetID, err)
	}
	if _, _, ok, _ := ui.CachePairIdentity(f.ID); !ok {
		t.Fatal("extension still absent after Ensure")
	}
	// And the folder can cache from here.
	if err := ui.SetCacheOffsets(f.ID, map[uint32]uint32{1: 64}); err != nil {
		t.Fatal(err)
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].CacheOffset != 64 {
		t.Errorf("offset after Ensure = %+v, want 64", msgs)
	}
}
