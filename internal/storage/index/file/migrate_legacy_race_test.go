package file

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestMigrateLegacyFilenamesConcurrentOpenersNoError reproduces #672:
// migrateLegacyFilenames runs unlocked on OpenFolder's first-open path, so
// two concurrent openers of the same not-yet-migrated folder can both pass
// the "native doesn't exist yet / legacy does exist" pre-checks before
// either has renamed anything. os.Rename is atomic — only one racer's
// rename actually succeeds — but before the fix the loser saw a hard ENOENT
// error instead of recognizing "someone else already migrated this." This
// drives many goroutines at the same legacy→native migration concurrently
// and asserts none of them ever return an error, with the migration
// genuinely completed at the end.
func TestMigrateLegacyFilenamesConcurrentOpenersNoError(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, LegacyIndexFileName)
	nativePath := filepath.Join(dir, IndexFileName)
	// A real index of ours under the legacy name. A placeholder would do for
	// the race itself, but the rename now refuses a file it cannot identify as
	// ours -- the legacy names are also another implementation's current names,
	// and renaming theirs takes their store away from them (#1574).
	seed := t.TempDir()
	seedIdx := New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: seed})
	if _, err := seedIdx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = seedIdx.Close()
	seeded, err := os.ReadFile(filepath.Join(seed, IndexFileName))
	if err != nil {
		t.Fatalf("read seeded index: %v", err)
	}
	if err := os.WriteFile(legacyPath, seeded, 0o600); err != nil {
		t.Fatalf("write legacy index: %v", err)
	}

	const openers = 50
	var wg sync.WaitGroup
	errs := make([]error, openers)
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = migrateLegacyFilenames(dir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: migrateLegacyFilenames returned error under concurrent migration: %v", i, err)
		}
	}
	if _, err := os.Stat(nativePath); err != nil {
		t.Fatalf("native index missing after concurrent migration: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy path gone after migration, stat returned: %v", err)
	}
}
