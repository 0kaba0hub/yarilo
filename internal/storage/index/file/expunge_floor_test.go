package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// seedFolderWithExpunge builds a folder with a message expunged, so the log
// holds an expunge record the fold will take away.
func seedFolderWithExpunge(t *testing.T) (*userIndex, uint64, string) {
	t.Helper()
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	for i := 1; i <= 2; i++ {
		uid, err := b.AllocateUID(f.ID)
		if err != nil {
			t.Fatalf("AllocateUID: %v", err)
		}
		if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "m.eml", Size: 10}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := b.ExpungeMessage(f.ID, 1); err != nil {
		t.Fatalf("ExpungeMessage: %v", err)
	}
	return b, f.ID, dir
}

// Every path that drops the log has to raise the floor. A path that forgets
// leaves Vanished answering an empty list for a window it cannot see, which a
// reader takes for "nothing was expunged" -- the phantom-message failure this
// extension exists to prevent.
//
// The paths are exercised through their public entry points rather than by
// calling the stamp helper, so a new truncation site that skips the stamp fails
// here instead of shipping.
func TestEveryFoldRaisesTheExpungeFloor(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T) (*userIndex, uint64)
		fold func(t *testing.T, u *userIndex, folderID uint64)
	}{
		{
			name: "OptimizeIndex",
			fold: func(t *testing.T, u *userIndex, folderID uint64) {
				if err := u.OptimizeIndex(folderID); err != nil {
					t.Fatalf("OptimizeIndex: %v", err)
				}
			},
		},
		{
			// The path nobody calls by name, and therefore the one most likely
			// to lose its stamp in a later refactor: rotation happens on its
			// own, so a fold without a floor would ship unnoticed.
			name: "automatic compaction",
			seed: func(t *testing.T) (*userIndex, uint64) {
				// Seeded without rotation, then reopened with thresholds a
				// single write crosses: the floor starts at zero and the fold
				// is triggered by an ordinary append, the way it happens in a
				// deployment.
				_, _, dir := seedFolderWithExpunge(t)
				home := testHome(dir, testUser)
				u := New(WithLogCompaction(1, 2, 0)).
					OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
				f, err := u.OpenFolder("INBOX", 1, "")
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				return u, f.ID
			},
			fold: func(t *testing.T, u *userIndex, folderID uint64) {
				if err := u.AppendMessage(folderID, &mailbox.MessageMeta{UID: 3, Filename: "m.eml", Size: 10}); err != nil {
					t.Fatalf("AppendMessage: %v", err)
				}
			},
		},
		{
			name: "ResetFolder",
			fold: func(t *testing.T, u *userIndex, folderID uint64) {
				if _, err := u.ResetFolder(folderID, []*mailbox.MessageMeta{
					{UID: 2, Filename: "m.eml", Size: 10},
				}); err != nil {
					t.Fatalf("ResetFolder: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var u *userIndex
			var folderID uint64
			if tc.seed != nil {
				u, folderID = tc.seed(t)
			} else {
				u, folderID, _ = seedFolderWithExpunge(t)
			}

			before, err := u.ExpungeFloor(folderID)
			if err != nil {
				t.Fatalf("ExpungeFloor: %v", err)
			}
			if before != 0 {
				t.Fatalf("floor is %d before anything was folded, want 0", before)
			}
			// The history is there while the log is.
			if uids, err := u.Vanished(folderID, 0); err != nil || len(uids) == 0 {
				t.Fatalf("expunge history missing before the fold: %v %v", uids, err)
			}

			tc.fold(t, u, folderID)

			after, err := u.ExpungeFloor(folderID)
			if err != nil {
				t.Fatalf("ExpungeFloor after: %v", err)
			}
			if after == 0 {
				t.Fatal("the fold dropped the log without raising the floor; Vanished now answers empty for a window it cannot see")
			}
		})
	}
}

// The floor survives a reopen, since it is what a later reader consults.
func TestExpungeFloorIsPersisted(t *testing.T) {
	u, folderID, dir := seedFolderWithExpunge(t)
	if err := u.OptimizeIndex(folderID); err != nil {
		t.Fatalf("OptimizeIndex: %v", err)
	}
	want, err := u.ExpungeFloor(folderID)
	if err != nil || want == 0 {
		t.Fatalf("floor after optimize: %d %v", want, err)
	}

	b2 := openIdx(dir, testUser)
	f2, err := b2.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := b2.ExpungeFloor(f2.ID)
	if err != nil {
		t.Fatalf("ExpungeFloor after reopen: %v", err)
	}
	if got != want {
		t.Errorf("floor after reopen = %d, want %d", got, want)
	}
}

// An index written before this extension existed reads as zero: nothing was
// folded away by a build that had a floor to record, so its log is the whole
// history and a reader may trust it.
func TestIndexWithoutTheExtensionReadsZero(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	fs := b.open[f.ID]
	fs.mu.Lock()
	for i, ext := range fs.file.Extensions {
		if ext.Name == extNameExpungeFloor {
			fs.file.Extensions = append(fs.file.Extensions[:i], fs.file.Extensions[i+1:]...)
			break
		}
	}
	floor := fs.expungeFloorLocked()
	fs.mu.Unlock()
	if floor != 0 {
		t.Errorf("an index with no floor extension reads %d, want 0", floor)
	}
}

// The floor may never move down: a lower floor promises history the log no
// longer has.
func TestExpungeFloorNeverGoesDown(t *testing.T) {
	u, folderID, _ := seedFolderWithExpunge(t)
	if err := u.OptimizeIndex(folderID); err != nil {
		t.Fatalf("first optimize: %v", err)
	}
	first, _ := u.ExpungeFloor(folderID)
	if err := u.OptimizeIndex(folderID); err != nil {
		t.Fatalf("second optimize: %v", err)
	}
	second, _ := u.ExpungeFloor(folderID)
	if second < first {
		t.Errorf("floor went from %d to %d", first, second)
	}
}
