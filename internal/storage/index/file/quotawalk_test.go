package file

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func counterVal(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}

// Re-opening a folder already open in this index makes no trip to the lock
// service. A usage walk opens every folder of the account and reads two header
// fields; the read has been lock-free since #1635, and the open beside it was
// still paying a round trip per folder -- 11 per walk on a real account, 71,962
// in one measured run (#1639).
//
// The lock server is real, because withDistLock runs unguarded without one and
// the counter this asserts on would never move.
func TestReopeningAnOpenFolderMakesNoLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)
	const user = "walk@example.com"
	ui := New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	// First opens are folder creation, which is a write and takes the lock by
	// right. The walk is what follows.
	names := []string{"INBOX", "Sent", "Drafts", "Trash", "Archive"}
	for _, n := range names {
		f, err := ui.OpenFolder(n, 0, "")
		if err != nil {
			t.Fatalf("first open of %s: %v", n, err)
		}
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10, VSize: 10}); err != nil {
			t.Fatalf("append to %s: %v", n, err)
		}
	}

	before := counterVal(t, metricLockAcquired, "shared", lockSiteOpenProbe)
	var bytes uint64
	for _, n := range names {
		f, err := ui.OpenFolder(n, 0, "")
		if err != nil {
			t.Fatalf("walk open of %s: %v", n, err)
		}
		b, _, err := ui.FolderVSize(f.ID)
		if err != nil {
			t.Fatalf("vsize of %s: %v", n, err)
		}
		bytes += b
	}
	got := counterVal(t, metricLockAcquired, "shared", lockSiteOpenProbe) - before

	// The walk has to have read something, or a walk that opened nothing would
	// pass this by making no trips at all.
	if bytes == 0 {
		t.Fatal("the walk summed 0 bytes, so it never reached a folder")
	}
	if got != 0 {
		t.Errorf("a walk over %d open folders made %v trips to the lock service, want 0: "+
			"every folder of every account is being probed on every usage count", len(names), got)
	}
}

// Whatever the open does without the lock, it still answers with what is on the
// disk: another writer's message is in the snapshot the next open returns.
func TestAWalkOpenStillSeesAnotherWritersMessage(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)
	const user = "walk2@example.com"
	open := func() *userIndex {
		return New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
			Username: user, Home: testHome(dir, user),
		}).(*userHandle).ui
	}
	reader, writer := open(), open()

	f, err := reader.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := writer.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := 1; uid <= 3; uid++ {
		if err := writer.AppendMessage(wf.ID, &mailbox.MessageMeta{
			UID: uint32(uid), Filename: fmt.Sprint(uid), Size: 100, VSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := reader.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID != f.ID {
		t.Fatalf("the second open made a new folder state: %d then %d", f.ID, snap.ID)
	}
	b, msgs, err := reader.FolderVSize(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if msgs != 3 || b != 300 {
		t.Errorf("the walk read %d messages and %d bytes, want 3 and 300: an open that skips the "+
			"lock is serving a snapshot from before the other session's writes", msgs, b)
	}
}
