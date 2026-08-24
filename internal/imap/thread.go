package imap

import (
	"fmt"
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

	var (
		unreadable  []uint32
		detached    []uint32
		lastReadErr error
	)
	threaded := make([]imapthread.Message, 0, len(msgs))
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		matched, raw, readErr := s.matchMessage(seqNum, m, criteria, needsBody)
		if readErr != nil {
			// Excluded, not silently matched: see matchMessage (#1283).
			unreadable = append(unreadable, m.UID)
			lastReadErr = readErr
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
		tm, headerErr := s.threadMessage(num, m, raw)
		if headerErr != nil {
			// Kept, not dropped: the message exists and the client can fetch
			// it, so removing it from the tree would be a second lie. It
			// threads as a conversation of one, which is wrong about its
			// ancestry -- hence the count.
			detached = append(detached, m.UID)
			lastReadErr = headerErr
		}
		threaded = append(threaded, tm)
	}

	// Reported once per command with counts and one example, never per
	// message, and at WARN because the tree the client is about to receive is
	// incomplete and nothing in it says so. A missing message does not leave a
	// hole in a thread reply: it leaves a smaller tree, which reads exactly
	// like a correct answer about a smaller mailbox.
	if len(unreadable) > 0 || len(detached) > 0 {
		metricUnreadable.WithLabelValues("thread").Add(float64(len(unreadable) + len(detached)))
		example := append(append([]uint32(nil), unreadable...), detached...)[0]
		slog.Warn("imap: thread could not read some messages; the tree is wrong about them",
			"user", s.userInfo.Username,
			"folder", s.folder.Name,
			// Excluded by a criterion that needed bytes nobody could read.
			"excluded", len(unreadable),
			// Threaded, but with no headers: a conversation of one.
			"detached", len(detached),
			"records_scanned", len(msgs),
			"threaded", len(threaded),
			"first_uid", example,
			"err", lastReadErr,
		)
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
func (s *session) threadMessage(num uint32, m *mailbox.MessageMeta, raw []byte) (imapthread.Message, error) {
	out := imapthread.Message{Num: num, Sent: m.InternalDate}
	if raw == nil {
		var err error
		if raw, err = s.readHeader(m); err != nil {
			return out, err
		}
	}
	if len(raw) == 0 {
		return out, nil
	}
	hdr, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		// A header the parser refuses still threads: it becomes a message with
		// no ancestry and no subject, which is a thread of its own rather than
		// a message missing from the reply. Reported, because that is a claim
		// about the mailbox and not about this message alone.
		return out, fmt.Errorf("imap/thread: parse header of uid %d: %w", m.UID, err)
	}
	out.MessageID = hdr.Header.Get("Message-Id")
	out.Subject = decodeSubject(hdr.Header.Get("Subject"))
	out.References = threadReferences(hdr.Header)
	// §2.2: the sent date is the Date header in UTC, and the internal date
	// only when there is no date to parse.
	if sent, derr := mail.ParseDate(hdr.Header.Get("Date")); derr == nil {
		out.Sent = sent.UTC()
	}
	return out, nil
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
func (s *session) readHeader(m *mailbox.MessageMeta) ([]byte, error) {
	if m.Filename == "" {
		// Nothing was ever stored for this record; that is not a read failure.
		return nil, nil
	}
	rc, err := s.fetchSelected(m)
	if err != nil {
		return nil, fmt.Errorf("imap/thread: open uid %d: %w", m.UID, err)
	}
	defer rc.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(rc, threadHeaderLimit))
	if err != nil && len(raw) == 0 {
		return nil, fmt.Errorf("imap/thread: read uid %d: %w", m.UID, err)
	}
	return raw, nil
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
