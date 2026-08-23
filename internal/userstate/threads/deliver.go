package threads

import (
	"bytes"
	"fmt"
	"net/mail"
	"strings"

	"github.com/yarilomail/yarilo/internal/threading"
)

// Recorder places a delivered message in a conversation and records it.
//
// The sidecar is per account, so the lock is per account: a conversation spans
// folders, and two deliveries into different folders would otherwise assign it
// two thread ids.
type Recorder struct {
	cache *Cache
}

func NewRecorder(cache *Cache) *Recorder {
	if cache == nil {
		cache = NewCache(DefaultIdle)
	}
	return &Recorder{cache: cache}
}

// Record threads one delivered message and returns the thread it belongs to.
//
// guid is the message's identity in this account, and it is also the thread id
// minted when there is nothing to join. Minting from the message rather than
// from randomness is what makes the sidecar REBUILDABLE: the migration step
// walking the same history has to arrive at the same thread ids, and that is
// the property the whole design leans on -- it is why there is no fsync on
// this path.
//
// It also makes the unmigrated account a special case of the same rule rather
// than different behaviour: today every message is its own thread, which is
// exactly threadID == guid.
func (r *Recorder) Record(user, path, guid string, raw []byte) (string, error) {
	if guid == "" {
		return "", fmt.Errorf("threads: delivery has no guid")
	}
	msg := parseHeaders(raw)
	msg.MessageID = firstOf(msg.MessageID)

	state, err := r.cache.Get(user, path)
	if err != nil {
		return "", err
	}
	placed := threading.Resolve(threading.Message{
		MessageID:  msg.MessageID,
		InReplyTo:  msg.InReplyTo,
		References: msg.References,
		Subject:    msg.Subject,
	}, state)

	threadID := placed.ThreadID
	if threadID == "" {
		threadID = guid
	}
	if err := Append(path, state, Placement{
		GUID:       guid,
		MessageID:  msg.MessageID,
		SubjectKey: placed.SubjectKey,
		ThreadID:   threadID,
		MergedFrom: placed.MergedFrom,
	}); err != nil {
		return "", err
	}
	r.cache.Note(user, path)
	return threadID, nil
}

type headers struct {
	MessageID  string
	InReplyTo  []string
	References []string
	Subject    string
}

// parseHeaders reads the four headers threading needs.
//
// A message whose headers cannot be parsed threads by nothing and becomes its
// own conversation -- which is the honest answer, and the same one it gets
// today. Refusing the delivery over it would be trading mail for metadata.
func parseHeaders(raw []byte) headers {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return headers{}
	}
	return headers{
		MessageID:  strings.TrimSpace(m.Header.Get("Message-ID")),
		InReplyTo:  messageIDs(m.Header.Get("In-Reply-To")),
		References: messageIDs(m.Header.Get("References")),
		Subject:    m.Header.Get("Subject"),
	}
}

// messageIDs splits a header that carries message identifiers. Angle brackets
// and commas are stripped, so "<a@x>, <b@x>" and "<a@x> <b@x>" read alike --
// clients emit both.
func messageIDs(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(v) {
		if id := strings.Trim(f, "<>,"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// firstOf keeps only the first identifier of a Message-ID header. A header
// carrying two is malformed; taking the first is what every reader does, and
// taking both would let one malformed message join two conversations.
func firstOf(v string) string {
	ids := messageIDs(v)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
