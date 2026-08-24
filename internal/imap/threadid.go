package imap

import (
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadIDs answers FETCH THREADID (RFC 8474) for one whole command.
//
// The conversations come from the account's threading sidecar -- the same one
// JMAP reads, deliberately. A second threading implementation on the IMAP side
// would be two answers to one question about one mailbox, and they would
// disagree the first time either was touched.
//
// Resolved once per command rather than per message, for two reasons. A
// FETCH 1:* (THREADID) over a large mailbox would otherwise stat the sidecar
// tens of thousands of times; and, worse, every message would answer from its
// own instant, so a merge landing mid-FETCH would return one command's worth
// of answers stitched from two states -- a conversation that never existed as
// a whole. This is the same reason Thread/get takes one Read per request.
//
// The lock is released before any of it reaches the wire: holding it across
// the response would park a delivery behind a slow client for the length of
// the fetch. Only the ids in hand are resolved, so the cost is the size of the
// fetch, not the size of the account.
//
// A missing entry means NIL on the wire, and NIL is not a failure: RFC 8474
// uses it for "the server cannot determine a thread", which is exactly the
// state of an account the migration step has not reached -- and what this
// returned for every message before threading existed.
func (s *session) threadIDs(msgs []*mailbox.MessageMeta) map[uint32]string {
	if s.srv.opts.Threads == nil || s.userInfo == nil || len(msgs) == 0 {
		return nil
	}
	path := threads.PathFor(s.userInfo)
	if path == "" {
		return nil
	}
	state, err := s.srv.opts.Threads.Get(s.userInfo.Username, path)
	if err != nil {
		// A sidecar that cannot be read is not a reason to fail a FETCH: the
		// conversation is metadata, the message is not.
		return nil
	}
	out := make(map[uint32]string, len(msgs))
	state.Read(func(v threads.View) {
		for _, m := range msgs {
			if m == nil || m.GUID == ([16]byte{}) {
				// Nothing to look the message up by. The GUID backfill is the
				// step that fixes this, and threading it under the zero id
				// would put every such message in one conversation.
				continue
			}
			if id, ok := v.ThreadOf(mailbox.FormatObjectID(m.GUID)); ok {
				out[m.UID] = id
			}
		}
	})
	return out
}
