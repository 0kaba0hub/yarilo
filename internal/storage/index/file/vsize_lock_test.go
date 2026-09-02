package file

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func exclusiveAcquisitions() float64 {
	total := 0.0
	for _, site := range []string{lockSiteOpenProbe, lockSiteRead, lockSiteWrite, lockSiteFallback} {
		total += testutil.ToFloat64(metricLockAcquired.WithLabelValues("exclusive", site))
	}
	return total
}

// Reading a folder's size takes no exclusive lock.
//
// It used to go through the write path, so counting a user's usage took an
// exclusive lock per folder to read two header fields -- over half of all
// exclusive acquisitions on the key, since a quota check runs before every save
// (#1634). The counter is the assertion: a total that only says "some locks
// were taken" would pass whichever path it used.
func TestFolderVSizeTakesNoExclusiveLock(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "u1@example.com")
	idx := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "u1@example.com", Home: home,
	})
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	// A message, so the folder has an aggregate worth reading and a lineage of
	// its own -- the property the lock-free path stands on.
	if err := idx.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Filename: "m1", Size: 10, VSize: 10}); err != nil {
		t.Fatal(err)
	}

	before := exclusiveAcquisitions()
	bytes, msgs, err := idx.FolderVSize(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := exclusiveAcquisitions() - before

	if msgs != 1 || bytes == 0 {
		t.Fatalf("the read returned %d bytes and %d messages, so it read nothing worth locking about", bytes, msgs)
	}
	if got != 0 {
		t.Errorf("reading the folder size took %v exclusive locks, want 0", got)
	}
}
