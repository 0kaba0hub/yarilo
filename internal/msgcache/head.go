package msgcache

import (
	"encoding/binary"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Head is the part of an envelope that ordering uses: SORT and THREAD compare
// the sent date, the base subject, and the FIRST MAILBOX of an address list --
// never a name, never a host, never a second address, and never Sender,
// Reply-To or Bcc at all.
//
// It exists because decoding the whole envelope for those five strings was
// 30.6% of every object a SORT (DATE) allocated on a ten-thousand-message
// account: six address lists nobody in that command would read (#1490). The
// measurement is on the issue; the number that justified this is a field one.
type Head struct {
	Date    time.Time
	Subject string
	// From, To and Cc carry the first address's mailbox part -- the local
	// part, which is what RFC 5256 §2.2 sorts an address key by.
	From, To, Cc string
	InReplyTo    []string
	MessageID    string
}

// decodeHead reads a Head out of the same bytes decodeEnvelope reads, skipping
// what it does not return.
//
// It walks the record rather than indexing into it: the fields are
// length-prefixed in sequence, so the only way to reach the message id is
// through the address lists. What it avoids is not the walk but the building
// -- a string per name, mailbox and host, and a slice per list.
func decodeHead(b []byte) (Head, bool) {
	var h Head
	if len(b) < 9 || b[0] != envelopeCodecVersion {
		return h, false
	}
	if unix := binary.LittleEndian.Uint64(b[1:9]); unix != 0 {
		h.Date = time.Unix(int64(unix), 0).UTC()
	}
	b = b[9:]
	var err error
	if h.Subject, b, err = getStr(b); err != nil {
		return h, false
	}
	// The six lists in the order encodeEnvelope writes them. Only three are
	// wanted, and of those only the first mailbox.
	for _, want := range []*string{&h.From, nil, nil, &h.To, &h.Cc, nil} {
		var ok bool
		if b, ok = firstMailboxOf(b, want); !ok {
			return h, false
		}
	}
	if len(b) < 4 {
		return h, false
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if n > 1<<16 {
		return h, false
	}
	if n > 0 {
		h.InReplyTo = make([]string, 0, n)
	}
	for i := uint32(0); i < n; i++ {
		var s string
		if s, b, err = getStr(b); err != nil {
			return h, false
		}
		h.InReplyTo = append(h.InReplyTo, s)
	}
	if h.MessageID, _, err = getStr(b); err != nil {
		return h, false
	}
	return h, true
}

// firstMailboxOf walks one encoded address list, writing the first address's
// mailbox part to want when want is non-nil, and returns the rest of the
// record. A nil want reads nothing and only skips.
func firstMailboxOf(b []byte, want *string) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if n > 1<<16 {
		return nil, false
	}
	for i := uint32(0); i < n; i++ {
		var ok bool
		// name
		if b, ok = skipStr(b); !ok {
			return nil, false
		}
		// mailbox: the one field anybody here asks for, and only from the
		// first address.
		if want != nil && i == 0 {
			var s string
			var err error
			if s, b, err = getStr(b); err != nil {
				return nil, false
			}
			*want = s
		} else if b, ok = skipStr(b); !ok {
			return nil, false
		}
		// host
		if b, ok = skipStr(b); !ok {
			return nil, false
		}
	}
	return b, true
}

// skipStr steps over one length-prefixed string without building it.
func skipStr(b []byte) ([]byte, bool) {
	if len(b) < 4 {
		return nil, false
	}
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if uint32(len(b)) < n {
		return nil, false
	}
	return b[n:], true
}

// Head returns the ordering fields of the cached envelope, or false on any of
// the three misses.
func (fc *Handle) Head(m *mailbox.MessageMeta) (Head, bool) {
	if fc == nil {
		return Head{}, false
	}
	data, ok := fc.read(m)[fc.envID]
	if !ok {
		return Head{}, false
	}
	return decodeHead(data)
}

// HeadAndReferences reads both in ONE pass over the message's record, for the
// same reason EnvelopeAndReferences does: asking separately walks the record
// chain twice, which was most of what a THREAD cost (#1461).
//
// The bool is "both halves are here", not "the head decoded". Threading needs
// the References as much as the envelope -- a head without them would thread
// the message by subject alone and put it in the wrong conversation -- so a
// caller that can only use the pair should not have to check two flags and
// remember which combination is safe.
func (fc *Handle) HeadAndReferences(m *mailbox.MessageMeta) (Head, []string, bool) {
	if fc == nil {
		return Head{}, nil, false
	}
	vals := fc.read(m)
	envData, ok := vals[fc.envID]
	if !ok {
		return Head{}, nil, false
	}
	h, ok := decodeHead(envData)
	if !ok {
		return Head{}, nil, false
	}
	refsData, cached := vals[fc.refsID]
	if !cached {
		return h, nil, false
	}
	if len(refsData) == 0 {
		return h, nil, true // cached, and the message has none
	}
	return h, splitRefs(refsData), true
}

// splitRefs is the one spelling of "how a References list is stored", shared so
// the two readers cannot drift apart.
func splitRefs(b []byte) []string {
	return strings.Split(string(b), "\n")
}

// HeadOf takes the ordering fields off a freshly-parsed envelope, for the miss
// path that had to open the message.
//
// It exists so "which fields ordering uses" is written once. Spelling it again
// at the call site would let the two answers drift, and the drift would show as
// a mailbox that sorts differently depending on whether its cache was warm.
func HeadOf(env *imaplib.Envelope) Head {
	h := Head{
		Date:      env.Date,
		Subject:   env.Subject,
		InReplyTo: env.InReplyTo,
		MessageID: env.MessageID,
	}
	first := func(addrs []imaplib.Address) string {
		if len(addrs) == 0 {
			return ""
		}
		return addrs[0].Mailbox
	}
	h.From, h.To, h.Cc = first(env.From), first(env.To), first(env.Cc)
	return h
}
