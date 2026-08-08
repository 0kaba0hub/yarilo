package file

import (
	"testing"

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
