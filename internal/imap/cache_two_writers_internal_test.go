package imap

import (
	"strconv"
	"sync"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Two writers, one pair, DIFFERENT content: each goroutine caches envelopes
// for its own messages through the full open-append-stamp window, many short
// windows each, from a common start barrier. Without the lock the appends
// interleave under two descriptors and a stamped offset resolves to a fully
// valid record for a DIFFERENT message -- which is exactly what the final
// per-UID assertion reads back. (The wire-level two-session test cannot
// distinguish this: both sessions there produce identical bytes.)
func TestFolderCache_TwoWritersStampTheirOwnMessages(t *testing.T) {
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	const n = 200 // messages per writer
	metas := make([]*mailbox.MessageMeta, 0, 2*n)
	for uid := uint32(1); uid <= 2*n; uid++ {
		m := &mailbox.MessageMeta{UID: uid}
		if err := idx.AppendMessage(f.ID, m); err != nil {
			t.Fatal(err)
		}
		metas = append(metas, m)
	}

	s := &session{
		srv:      &Server{opts: Options{}},
		userInfo: &mailbox.UserInfo{Username: "u"},
		folder:   f,
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			// One message per window: maximal interleaving of the guarded
			// open-append-stamp sections between the two writers.
			for i := 0; i < n; i++ {
				m := metas[w*n+i]
				fc := s.openFolderCache(idx, f.ID)
				if fc == nil {
					t.Error("cache unavailable")
					return
				}
				fc.store(m, &imaplib.Envelope{Subject: "msg-" + strconv.Itoa(int(m.UID))})
				fc.close()
			}
		}(w)
	}
	close(start)
	wg.Wait()

	// Every stamped offset must resolve to its OWN message.
	msgs, err := idx.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc := s.openFolderCache(idx, f.ID)
	if fc == nil {
		t.Fatal("cache unavailable for verification")
	}
	defer fc.close()
	verified := 0
	for _, m := range msgs {
		if m.CacheOffset == 0 {
			t.Errorf("uid %d: no offset stamped", m.UID)
			continue
		}
		env := fc.envelope(m)
		if env == nil {
			t.Errorf("uid %d: offset %d resolves to no envelope", m.UID, m.CacheOffset)
			continue
		}
		if want := "msg-" + strconv.Itoa(int(m.UID)); env.Subject != want {
			t.Errorf("uid %d: offset %d carries %q, want %q -- a foreign message's record", m.UID, m.CacheOffset, env.Subject, want)
		}
		verified++
	}
	if verified != 2*n {
		t.Errorf("verified %d of %d", verified, 2*n)
	}
}
