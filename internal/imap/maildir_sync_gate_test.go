package imap

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The real driver, not a double: the gate under test is the change token the
// driver computes from cur/ and new/, so a fake token would only test the
// caller's map.
func gateSetup(t *testing.T) (*session, *nsHandle, string) {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: filepath.Join(root, "x.com", "u")}
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	idx := fileidx.New().OpenUser(info)
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("open folder: %v", err)
	}
	s := newGateSession(info, &syncTokenCache{maxEntries: 8})
	// No MailPath from userdb, so INBOX is the maildir root <home>/Maildir.
	return s, &nsHandle{box: box, idx: idx, location: info.Home}, filepath.Join(info.Home, "Maildir")
}

// newGateSession is a logged-in session reconciling against the given cache —
// the seam the process-wide cache is tested through, since a second session for
// the same user is exactly the case it exists for.
func newGateSession(info *mailbox.UserInfo, cache *syncTokenCache) *session {
	return &session{
		srv:               &Server{opts: Options{MaildirSyncOnSelect: true}},
		userInfo:          info,
		maildirSyncTokens: cache,
	}
}

// settle backdates cur/ and new/ to a fixed instant. Without it every token
// carries the same-second dirty nonce — a folder written by the test itself is
// always "changed" — and the gate could never be observed holding.
func settle(t *testing.T, inbox string, at time.Time) {
	t.Helper()
	for _, sub := range []string{"cur", "new"} {
		if err := os.Chtimes(filepath.Join(inbox, sub), at, at); err != nil {
			t.Fatalf("chtimes %s: %v", sub, err)
		}
	}
}

// deliverOutOfBand drops a file into cur/ the way a second MUA does, then
// backdates the directory to a distinct settled instant: the scan that follows
// is provoked by the moved mtime, not by the same-second dirty rule.
func deliverOutOfBand(t *testing.T, inbox, name string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(inbox, "cur", name), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	settle(t, inbox, at)
}

func messageCount(t *testing.T, h *nsHandle) int {
	t.Helper()
	f, err := h.idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	msgs, err := h.idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	return len(msgs)
}

// The pair the gate has to get right, both counters pinned in both directions:
// an untouched folder must cost no walk however often it is selected, and a
// file that appeared out of band must still be picked up. Either half alone is
// satisfiable by a broken gate — one by never scanning, the other by always
// scanning (#1265).
func TestMaildirSyncGateScansOnlyWhenTheFolderChanged(t *testing.T) {
	settled := time.Unix(1700000000, 0)

	tests := []struct {
		name string
		// change is applied before the second reconcile pass.
		change          func(t *testing.T, inbox string)
		wantScanned     float64
		wantSkipped     float64
		wantMessages    int
		wantViewChanged bool
	}{
		{
			name:         "untouched folder is not walked again",
			change:       func(*testing.T, string) {},
			wantScanned:  0,
			wantSkipped:  1,
			wantMessages: 0,
		},
		{
			name: "file dropped into cur out of band is imported",
			change: func(t *testing.T, inbox string) {
				deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host:2,S", settled.Add(time.Minute))
			},
			wantScanned:     1,
			wantSkipped:     0,
			wantMessages:    1,
			wantViewChanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, h, inbox := gateSetup(t)
			settle(t, inbox, settled)

			// The first pass has no cached token and always walks; it is the
			// baseline, not part of the assertion.
			s.reconcileFolder(h, "INBOX")
			scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

			tc.change(t, inbox)

			if got := s.reconcileFolder(h, "INBOX"); got != tc.wantViewChanged {
				t.Errorf("reconcileFolder = %v, want %v", got, tc.wantViewChanged)
			}
			if got := syncCount(t, "scanned") - scanned; got != tc.wantScanned {
				t.Errorf("scans = %v, want %v", got, tc.wantScanned)
			}
			if got := syncCount(t, "skipped") - skipped; got != tc.wantSkipped {
				t.Errorf("skips = %v, want %v", got, tc.wantSkipped)
			}
			if got := messageCount(t, h); got != tc.wantMessages {
				t.Errorf("messages = %d, want %d", got, tc.wantMessages)
			}
		})
	}
}

// The quiet case at workload length: N re-selects of a folder nobody touched
// must cost N stats and zero walks. A gate that holds once and then lets go —
// a token overwritten with a nonce, a cache keyed by something that varies —
// passes the single-pass check and fails here.
func TestMaildirSyncGateHoldsAcrossManyQuietSelects(t *testing.T) {
	const selects = 20

	s, h, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))

	s.reconcileFolder(h, "INBOX")
	scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

	for i := 0; i < selects; i++ {
		s.reconcileFolder(h, "INBOX")
	}

	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("%v walks over %d quiet selects, want 0", got, selects)
	}
	if got := syncCount(t, "skipped") - skipped; got != selects {
		t.Errorf("skips = %v, want %d", got, selects)
	}
}
