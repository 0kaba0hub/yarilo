package quota_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// A folder visible in storage but never indexed must contribute nothing and,
// crucially, must not be indexed as a side effect of being counted: a policy
// service observes, it does not establish state (#993). Creating the index here
// would race a session pod's locked createFresh on the same shared storage.
func TestCountUsageDoesNotIndexAnUnindexedFolder(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home}

	idx := file.New(file.WithNoCreate()).OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck

	usage := quota.CountUsage(idx, []string{"INBOX", "NeverIndexed"}, quota.Limits{})
	if usage.StorageBytes != 0 || usage.Messages != 0 {
		t.Errorf("usage = %+v, want zero for folders with no index", usage)
	}

	// The acceptance criterion: no index file appeared.
	var created []string
	err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(path) == "yarilo.index" {
			created = append(created, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("counting created index files: %v", created)
	}
}
