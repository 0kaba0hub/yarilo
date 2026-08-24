package imap

import (
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadIDOf answers FETCH THREADID (RFC 8474) from the account's threading
// sidecar.
//
// The same sidecar JMAP reads, deliberately. A second threading implementation
// on the IMAP side would be two answers to one question about one mailbox, and
// they would disagree the first time either was touched -- so IMAP does not
// compute conversations, it reads the ones the delivery recorded.
//
// An empty answer is NIL on the wire, and NIL is not a failure: RFC 8474 uses
// it for "the server cannot determine a thread", which is exactly the state of
// an account the migration step has not reached -- and what this returned for
// every message before threading existed.
func (s *session) threadIDOf(m *mailbox.MessageMeta) string {
	if s.srv.opts.Threads == nil || m == nil || s.userInfo == nil {
		return ""
	}
	if m.GUID == ([16]byte{}) {
		// Nothing to look the message up by. The GUID backfill is the step
		// that fixes this, and threading it under the zero id would put every
		// such message in one conversation.
		return ""
	}
	path := threads.PathFor(s.userInfo)
	if path == "" {
		return ""
	}
	state, err := s.srv.opts.Threads.Get(s.userInfo.Username, path)
	if err != nil {
		// A sidecar that cannot be read is not a reason to fail a FETCH: the
		// conversation is metadata, the message is not.
		return ""
	}
	id, ok := state.ThreadOfGUID(mailbox.FormatObjectID(m.GUID))
	if !ok {
		return ""
	}
	return id
}
