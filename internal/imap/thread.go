package imap

import (
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/imapthread"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// threadHeaderLimit caps how much of a message is read to find its threading
// headers. They live in the header, and a message whose header is larger than
// this is malformed rather than unusual.
const threadHeaderLimit = 256 << 10

// Thread implements the THREAD command (RFC 5256).
//
// It computes the tree per command from message headers and does NOT read the
// threading sidecar that answers FETCH THREADID and JMAP. The two can group
// edge cases differently: RFC 5256's base subject rules know a fixed,
// English-shaped set of prefixes, while the sidecar follows the mail. That
// divergence is deliberate -- the capability announces conformance to this
// specification, and clients test it -- and is recorded in INTERNALS.md §23.
func (s *session) Thread(kind imapserver.NumKind, alg imaplib.ThreadAlgorithm, criteria *imaplib.SearchCriteria) ([]imaplib.ThreadNode, error) {
	slog.Debug("imap: command", "sid", s.sid, "cmd", "Thread", "alg", string(alg))
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	if err := s.requireRightOnSelected(mailbox.RightRead); err != nil {
		return nil, err
	}
	criteria = s.substituteSearchRes(criteria)

	msgs, err := readMessages(s.folderIdx(), s.folder.ID)
	if err != nil {
		return nil, err
	}
	needsBody := searchCriteriaHasBody(criteria)

	threaded := make([]imapthread.Message, 0, len(msgs))
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		matched, raw, readErr := s.matchMessage(seqNum, m, criteria, needsBody)
		if readErr != nil {
			// Excluded, not silently matched: see matchMessage (#1283).
			slog.Debug("imap: thread skipped unreadable message",
				"sid", s.sid, "user", s.userInfo.Username, "uid", m.UID, "err", readErr)
			continue
		}
		if !matched {
			continue
		}
		if criteria.ModSeq != nil && m.ModSeq < criteria.ModSeq.ModSeq {
			continue
		}

		num := seqNum
		if kind == imapserver.NumKindUID {
			num = m.UID
		}
		threaded = append(threaded, s.threadMessage(num, m, raw))
	}

	switch alg {
	case imaplib.ThreadReferences:
		return imapthread.References(threaded), nil
	case imaplib.ThreadOrderedSubject:
		return imapthread.OrderedSubject(threaded), nil
	default:
		// The dispatcher refuses algorithms this server did not announce, so
		// reaching here means the two disagree.
		return nil, &imaplib.Error{
			Type: imaplib.StatusResponseTypeBad,
			Text: "Unsupported threading algorithm " + string(alg),
		}
	}
}

// threadMessage reads the headers threading needs. raw is whatever the match
// already read, so a search that had to open the message does not open it
// again.
func (s *session) threadMessage(num uint32, m *mailbox.MessageMeta, raw []byte) imapthread.Message {
	out := imapthread.Message{Num: num, Sent: m.InternalDate}
	if raw == nil {
		raw = s.readHeader(m)
	}
	if len(raw) == 0 {
		return out
	}
	hdr, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		// A header the parser refuses still threads: it becomes a message with
		// no ancestry and no subject, which is a thread of its own rather than
		// a message missing from the reply.
		return out
	}
	out.MessageID = hdr.Header.Get("Message-Id")
	out.Subject = decodeSubject(hdr.Header.Get("Subject"))
	out.References = threadReferences(hdr.Header)
	// §2.2: the sent date is the Date header in UTC, and the internal date
	// only when there is no date to parse.
	if sent, derr := mail.ParseDate(hdr.Header.Get("Date")); derr == nil {
		out.Sent = sent.UTC()
	}
	return out
}

// threadReferences follows §4's rule about where ancestry comes from:
// References when it names anything at all, and only then the FIRST id in
// In-Reply-To -- that header has been observed carrying addresses after the
// id, and there is no heuristic that tells them apart.
func threadReferences(hdr mail.Header) []string {
	if refs := messageIDList(hdr.Get("References")); len(refs) > 0 {
		return refs
	}
	if refs := messageIDList(hdr.Get("In-Reply-To")); len(refs) > 0 {
		return refs[:1]
	}
	return nil
}

// messageIDList takes the bracketed ids and nothing else: comments, addresses
// and stray words between them are not identities.
func messageIDList(v string) []string {
	var out []string
	for {
		open := strings.IndexByte(v, '<')
		if open < 0 {
			return out
		}
		end := strings.IndexByte(v[open:], '>')
		if end < 0 {
			return out
		}
		if id := strings.TrimSpace(v[open : open+end+1]); len(id) > 2 {
			out = append(out, id)
		}
		v = v[open+end+1:]
	}
}

// readHeader reads a bounded prefix of the message: threading needs the header
// and nothing else, and a THREAD over a large mailbox would otherwise read
// every byte of every message in it.
func (s *session) readHeader(m *mailbox.MessageMeta) []byte {
	if m.Filename == "" {
		return nil
	}
	rc, err := s.fetchSelected(m)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(rc, threadHeaderLimit))
	if err != nil && len(raw) == 0 {
		return nil
	}
	return raw
}

// decodeSubject performs step (1) of RFC 5256 §2.1: encoded-words become
// UTF-8 before any prefix is looked for, because "=?utf-8?B?UmU6IA==?=" is a
// reply and its raw form is not.
func decodeSubject(v string) string {
	dec := new(mime.WordDecoder)
	out, err := dec.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}
