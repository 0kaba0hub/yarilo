package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestReadPathSerializesAgainstConcurrentLockHolder is the #647 regression:
// withFolderRO's reload must be serialized against writers via the same
// cross-process lock the write path takes. Before the fix the read path took
// only the in-process fs.mu and could interleave with another process's
// lock-holding compaction, poisoning the shared in-memory header (NextUID
// regression). Two clients on one embedded lock server stand in for two pods:
// client B holds the folder's X lock (an in-progress compaction), and a
// read-only op on the index wired to client A must block until B releases.
func TestReadPathSerializesAgainstConcurrentLockHolder(t *testing.T) {
	sock := holdsTestSocket(t)
	clientA := newHoldsClient(t, sock)
	clientB := newHoldsClient(t, sock)

	const username = "dave@example.com"
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: username, Home: home}
	idxA := file.New(file.WithLocker(clientA)).OpenUser(user)
	t.Cleanup(func() { _ = idxA.Close() })

	folder, err := idxA.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	uid, err := idxA.AllocateUID(folder.ID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := idxA.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Filename: "1.eml"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Client B (another pod) grabs the folder's X lock, standing in for an
	// in-progress compaction holding it.
	key := locks.MailboxKey(username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	held, err := clientB.Lock(locks.WithSite(ctx, "write"), key, "podB/0/"+username+"/sess2", 30*time.Second)
	if err != nil {
		t.Fatalf("client B lock: %v", err)
	}
	if !clientB.HoldsResource(key) {
		t.Fatal("client B does not hold the lock it just acquired")
	}

	// A read-only op on idxA must block acquiring the same key that B holds.
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, e := idxA.GetMessages(folder.ID, mailbox.SeqSet{})
		done <- e
	}()
	<-started

	select {
	case <-done:
		t.Fatal("read-only GetMessages returned while another client held the folder lock — reload was NOT serialized (#647)")
	case <-time.After(400 * time.Millisecond):
		// Still blocked on the distributed lock — the fix is in effect.
	}

	// Release B's lock; A's read must now complete promptly.
	if err := clientB.Unlock(ctx, held.ID); err != nil {
		t.Fatalf("client B unlock: %v", err)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("GetMessages after lock release: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read-only GetMessages did not complete after the lock was released")
	}
}
