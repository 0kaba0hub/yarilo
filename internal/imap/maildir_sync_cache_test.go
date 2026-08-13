package imap

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The case the whole change is for: a client that logs in per cycle. The
// session-scoped cache walked cur/ and new/ on the first SELECT of every
// session, so the gate never reached a workload that reconnects — which is
// every benchmark run and every phone.
func TestFreshSessionReusesTheProcessCache(t *testing.T) {
	first, h, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))

	first.reconcileFolder(h, "INBOX")
	scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

	// A new session for the same user, as a reconnect produces.
	for i := 0; i < 5; i++ {
		next := newGateSession(first.userInfo, first.maildirSyncTokens)
		next.reconcileFolder(h, "INBOX")
	}

	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("%v walks over 5 fresh sessions on an unchanged folder, want 0", got)
	}
	if got := syncCount(t, "skipped") - skipped; got != 5 {
		t.Errorf("skips = %v, want 5", got)
	}
}

// The other direction, at session granularity: a longer-lived cache must not
// outlive the truth. A file that appeared out of band between two logins is
// still picked up, because the token — not the cache entry — is what decides.
func TestFreshSessionStillSeesOutOfBandDelivery(t *testing.T) {
	settled := time.Unix(1700000000, 0)
	first, h, inbox := gateSetup(t)
	settle(t, inbox, settled)
	first.reconcileFolder(h, "INBOX")

	deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host:2,S", settled.Add(time.Minute))

	next := newGateSession(first.userInfo, first.maildirSyncTokens)
	if !next.reconcileFolder(h, "INBOX") {
		t.Fatal("a fresh session missed a file delivered between logins")
	}
	if got := messageCount(t, h); got != 1 {
		t.Errorf("messages = %d, want 1", got)
	}
}

// One process serves many users, and two of them may hold the same folder name
// at different storage roots. Keying by folder name alone would let one user's
// token answer for another's folder — a skip of a scan that was never proven.
func TestCacheKeyKeepsUsersAndLocationsApart(t *testing.T) {
	tests := []struct {
		name string
		a, b [3]string // username, location, folder
		want bool      // same key
	}{
		{"same user, same folder, same location", [3]string{"u@x.com", "/mail/u", "INBOX"}, [3]string{"u@x.com", "/mail/u", "INBOX"}, true},
		{"different users", [3]string{"u@x.com", "/mail/u", "INBOX"}, [3]string{"v@x.com", "/mail/u", "INBOX"}, false},
		{"same user, different storage roots", [3]string{"u@x.com", "/mail/u", "INBOX"}, [3]string{"u@x.com", "/public", "INBOX"}, false},
		{"same user, different folders", [3]string{"u@x.com", "/mail/u", "INBOX"}, [3]string{"u@x.com", "/mail/u", "Sent"}, false},
		// The separator has to be one no field can contain, or the boundary
		// between fields is guessable from their contents.
		{"field boundary is not forgeable", [3]string{"u", "x", "INBOX"}, [3]string{"u\x00x", "", "INBOX"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ka := syncTokenKey(tc.a[0], tc.a[1], tc.a[2])
			kb := syncTokenKey(tc.b[0], tc.b[1], tc.b[2])
			if (ka == kb) != tc.want {
				t.Errorf("keys %q / %q: same = %v, want %v", ka, kb, ka == kb, tc.want)
			}
		})
	}
}

// Two users must not answer for each other even when their folder names match,
// asked through the reconcile path rather than through the key function.
func TestSecondUserIsNotAnsweredByTheFirstsToken(t *testing.T) {
	first, h, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))
	first.reconcileFolder(h, "INBOX")
	scanned := syncCount(t, "scanned")

	other := newGateSession(&mailbox.UserInfo{
		Username: "v@x.com",
		Home:     filepath.Join(t.TempDir(), "x.com", "v"),
	}, first.maildirSyncTokens)
	other.reconcileFolder(h, "INBOX")

	if got := syncCount(t, "scanned") - scanned; got != 1 {
		t.Errorf("scans for a second user = %v, want 1 (its own token was never proven)", got)
	}
}

// Overflow drops the map rather than expiring entries by age. The bound has to
// cost a scan, never a wrong skip: after the drop the folder is walked again,
// and nothing is served from a cleared cache.
func TestCacheOverflowCostsAScanNotCorrectness(t *testing.T) {
	c := &syncTokenCache{maxEntries: 4}
	c.put("a", "t")
	for i := 0; i < 4; i++ {
		c.put(string(rune('b'+i)), "t")
	}
	if _, ok := c.get("a"); ok {
		t.Error("entry survived the overflow drop; the bound is not bounding")
	}
	if len(c.tokens) > c.maxEntries {
		t.Errorf("%d entries held, cap is %d", len(c.tokens), c.maxEntries)
	}
	if tok, ok := c.get(string(rune('b' + 3))); !ok || tok != "t" {
		t.Error("the entry that triggered the drop was not kept")
	}
}
