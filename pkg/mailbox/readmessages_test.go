package mailbox

import "testing"

// lockedOnly is an index without the optional capability — every index that
// cannot prove its files' freshness looks like this.
type lockedOnly struct {
	UserIndex
	locked int
}

func (l *lockedOnly) GetMessages(_ uint64, _ SeqSet) ([]*MessageMeta, error) {
	l.locked++
	return nil, nil
}

// capable additionally offers the lock-free read.
type capable struct {
	lockedOnly
	unlocked int
}

func (c *capable) GetMessagesUnlocked(_ uint64, _ SeqSet) ([]*MessageMeta, error) {
	c.unlocked++
	return nil, nil
}

// The adapter has to prefer the lock-free read where it exists and fall back
// where it does not. Both halves: an adapter that always fell back would look
// correct on the second index and quietly undo the whole change on the first.
func TestReadMessagesPrefersTheUnlockedReadAndFallsBack(t *testing.T) {
	c := &capable{}
	if _, err := ReadMessages(c, 1, SeqSet{}); err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if c.unlocked != 1 || c.locked != 0 {
		t.Errorf("capable index: unlocked=%d locked=%d, want 1/0", c.unlocked, c.locked)
	}

	l := &lockedOnly{}
	if _, err := ReadMessages(l, 1, SeqSet{}); err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if l.locked != 1 {
		t.Errorf("index without the capability: locked=%d, want 1", l.locked)
	}
}
