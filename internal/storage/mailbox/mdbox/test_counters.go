package mdbox

import (
	"sync/atomic"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
)

// Test seams: reading a message by uid must cost neither a directory listing
// nor a walk of the map, and that is counted rather than argued (#1700).
var countDirReads atomic.Int64

// SetTestCounters zeroes both and returns their readers.
func SetTestCounters() (dirReads, mapWalks func() int) {
	countDirReads.Store(0)
	mdboxmap.ResetGUIDWalks()
	return func() int { return int(countDirReads.Load()) }, mdboxmap.GUIDWalks
}
