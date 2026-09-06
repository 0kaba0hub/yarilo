package imap

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func starMsgs(uids ...uint32) ([]*mailbox.MessageMeta, map[uint32]uint32) {
	msgs := make([]*mailbox.MessageMeta, 0, len(uids))
	seq := make(map[uint32]uint32, len(uids))
	for i, uid := range uids {
		msgs = append(msgs, &mailbox.MessageMeta{UID: uid, Filename: "m"})
		seq[uid] = uint32(i + 1)
	}
	return msgs, seq
}

// A bare "*" is the largest number in the mailbox (RFC 3501 §9); unresolved it
// matches nothing.
func TestResolveStarUIDSet(t *testing.T) {
	msgs, seq := starMsgs(3, 7, 16)
	tests := []struct {
		name    string
		in      imaplib.UIDSet
		matches []uint32
		misses  []uint32
	}{
		{
			name:    "bare star is the largest uid",
			in:      imaplib.UIDSet{{Start: 0, Stop: 0}},
			matches: []uint32{16},
			misses:  []uint32{3, 7},
		},
		{
			name:    "open range keeps matching from its start",
			in:      imaplib.UIDSet{{Start: 7, Stop: 0}},
			matches: []uint32{7, 16},
			misses:  []uint32{3},
		},
		{
			name:    "closed range is untouched",
			in:      imaplib.UIDSet{{Start: 3, Stop: 7}},
			matches: []uint32{3, 7},
			misses:  []uint32{16},
		},
		{
			name:    "star inside a longer set",
			in:      imaplib.UIDSet{{Start: 3, Stop: 3}, {Start: 0, Stop: 0}},
			matches: []uint32{3, 16},
			misses:  []uint32{7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveStar(tt.in, msgs, seq)
			for _, uid := range tt.matches {
				if !numSetContains(got, seq[uid], imaplib.UID(uid)) {
					t.Errorf("uid %d should match %s", uid, tt.in.String())
				}
			}
			for _, uid := range tt.misses {
				if numSetContains(got, seq[uid], imaplib.UID(uid)) {
					t.Errorf("uid %d should not match %s", uid, tt.in.String())
				}
			}
		})
	}
}

func TestResolveStarSeqSet(t *testing.T) {
	msgs, seq := starMsgs(3, 7, 16)
	got := resolveStar(imaplib.SeqSet{{Start: 0, Stop: 0}}, msgs, seq)
	if !numSetContains(got, seq[16], imaplib.UID(16)) {
		t.Error("bare star should match the last sequence number")
	}
	if numSetContains(got, seq[3], imaplib.UID(3)) {
		t.Error("bare star should not match the first sequence number")
	}
}

// An empty mailbox has no largest number, so the set is left as it is and
// simply matches nothing.
func TestResolveStarEmptyMailbox(t *testing.T) {
	got := resolveStar(imaplib.UIDSet{{Start: 0, Stop: 0}}, nil, nil)
	if numSetContains(got, 1, imaplib.UID(1)) {
		t.Error("star matched in an empty mailbox")
	}
}

// resolveStar must not write through to the caller's set: the session reuses it.
func TestResolveStarDoesNotMutateInput(t *testing.T) {
	msgs, seq := starMsgs(5)
	in := imaplib.UIDSet{{Start: 0, Stop: 0}}
	resolveStar(in, msgs, seq)
	if in[0].Start != 0 || in[0].Stop != 0 {
		t.Errorf("input set mutated to %v", in[0])
	}
}
