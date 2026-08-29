package imap

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/quota"
)

// The post-commit usage is the cached total plus what this session just did.
func TestUsageAfterDeltaAppliesTheChange(t *testing.T) {
	for _, tc := range []struct {
		name                string
		dBytes, dMessages   int64
		wantBytes, wantMsgs int64
	}{
		{"a delivery adds", 4096, 1, 104096, 11},
		{"an expunge subtracts", -4096, -1, 95904, 9},
		{"more removed than counted cannot go below zero", -1 << 40, -100, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{
				quotaCacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10},
				quotaCacheAt:    time.Now(),
			}
			got, ok := s.usageAfterDelta(tc.dBytes, tc.dMessages)
			if !ok {
				t.Fatal("a fresh cache refused to answer, so every commit falls back to the sweep")
			}
			if got.StorageBytes != tc.wantBytes || got.Messages != tc.wantMsgs {
				t.Errorf("usage %+v, want %d bytes and %d messages", got, tc.wantBytes, tc.wantMsgs)
			}
		})
	}
}

// A stale cache is not built on. Otherwise the total would drift from whatever
// it last really was, for as long as the session kept changing things.
func TestUsageAfterDeltaRefusesAStaleCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"never counted", time.Time{}},
		{"counted longer ago than the TTL", time.Now().Add(-2 * quotaCacheTTL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{quotaCacheUsage: quota.Usage{StorageBytes: 100000}, quotaCacheAt: tc.at}
			if _, ok := s.usageAfterDelta(4096, 1); ok {
				t.Error("built on a cache it should not trust")
			}
		})
	}
}

// The delta must not extend the cache's life.
//
// This is what bounds the drift. Refreshing the timestamp on every change would
// keep the cached total alive for as long as the load lasts -- a session
// expunging steadily would never re-count, and another session's delivery would
// never appear in its total. Leaving the timestamp alone means the total is
// rebuilt one TTL after its last real count, so what the deltas accumulate
// cannot outlive a second.
func TestTheDeltaDoesNotExtendTheCacheLifetime(t *testing.T) {
	at := time.Now().Add(-quotaCacheTTL / 2)
	s := &session{quotaCacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10}, quotaCacheAt: at}

	for i := 0; i < 5; i++ {
		if _, ok := s.usageAfterDelta(-100, -1); !ok {
			t.Fatalf("refused at iteration %d", i)
		}
	}
	if !s.quotaCacheAt.Equal(at) {
		t.Errorf("the cache timestamp moved from %v to %v: deltas are keeping it alive, and a real count may never happen again",
			at, s.quotaCacheAt)
	}
}

// A change whose size the caller does not know invalidates the cache, so the
// next read counts for real.
//
// One path still arrives without a size: the imapsieve expunge, which is given
// a UID and a filename and no message record. Carrying the stale total forward
// there would let it drift with no bound at all, which is the one thing the TTL
// was protecting against.
func TestAnUnknownSizeLeavesNothingToBuildOn(t *testing.T) {
	s := &session{
		quotaCacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10},
		quotaCacheAt:    time.Now(),
	}
	// What emitMailboxChangeSized does when vsize is 0: no delta is attempted,
	// and the cache is dropped.
	s.quotaChanged()

	if _, ok := s.usageAfterDelta(-100, -1); ok {
		t.Error("the cache survived a change of unknown size, so the total drifts with nothing bounding it")
	}
}
