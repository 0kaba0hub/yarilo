package imap

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"strings"

	"github.com/emersion/go-message/textproto"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/imapthread"
	"github.com/yarilomail/yarilo/internal/msgcache"
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

	threaded, err := s.scanForOrdering(kind, criteria, "thread", orderingNeeds{envelope: true, refs: true})
	if err != nil {
		return nil, err
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

// scanForOrdering collects the searched messages with the headers that
// ordering needs -- the shared front half of THREAD and SORT, so that the two
// select the same messages and account for unreadable ones the same way.
//
// command names the caller on the shared counter and in the warning, which is
// the only thing that differs between them.
// orderingNeeds is what a command actually has to read per message.
//
// Both fields false means the index already holds the answer -- ARRIVAL is the
// internal date, SIZE is the size -- and the message is never opened. Measured
// on 10 442 messages before this existed: SORT (ARRIVAL) cost 850ms against
// SEARCH ALL's 10ms over the same mailbox, all of it spent opening files for
// headers nobody asked for (#1461).
type orderingNeeds struct {
	// envelope: Subject, sent date, or an address. All are in ENVELOPE, which
	// the message cache holds and FETCH already reads without opening
	// anything (#1030).
	envelope bool
	// refs: the References header, the one threading field ENVELOPE does not
	// carry -- so it is still the one reason to open a message.
	refs bool
}

// sortNeeds reads the criteria of a SORT command. A key that the index answers
// asks for nothing.
func sortNeeds(criteria []imaplib.SortCriterion) orderingNeeds {
	var n orderingNeeds
	for _, c := range criteria {
		switch c.Key {
		case imaplib.SortKeyArrival, imaplib.SortKeySize:
		default:
			n.envelope = true
		}
	}
	return n
}

func (s *session) scanForOrdering(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, command string, needs orderingNeeds) ([]imapthread.Message, error) {
	msgs, err := readMessages(s.folderIdx(), s.folder.ID)
	if err != nil {
		return nil, err
	}
	needsBody := searchCriteriaHasBody(criteria)

	var (
		unreadable []uint32
		detached   []uint32
		// byReason splits both lists between a message that is gone and one
		// that is there and unreadable: only the second says the store is
		// damaged.
		byReason    = map[string]int{}
		lastReadErr error
	)
	// One handle per command, as FETCH opens one: misses are parsed and
	// written back, so the second ordering command over a mailbox pays
	// nothing for what the first one had to read.
	var envCache *msgcache.Handle
	if needs.envelope || needs.refs {
		envCache = msgcache.Open(s.folderIdx(), s.folder.ID, msgcache.Options{
			Locker:    s.srv.opts.Locker,
			User:      s.userInfo.Username,
			SessionID: s.userInfo.SessionID,
			Folder:    s.folder.Name,
			TraceID:   s.sid,
		})
		// This command walks every matched message, so the cache is read in
		// one pass rather than a record at a time: per-record reads put 30% of
		// a THREAD's CPU into pread on a large mailbox (#1461).
		envCache.Preload()
		defer envCache.Close()
	}

	out := make([]imapthread.Message, 0, len(msgs))
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		matched, raw, readErr := s.matchMessage(seqNum, m, criteria, needsBody)
		if readErr != nil {
			// Excluded, not silently matched: see matchMessage (#1283).
			unreadable = append(unreadable, m.UID)
			byReason[unreadableReason(readErr)]++
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
		one, headerErr := s.orderingMessage(num, m, raw, needs, envCache)
		if headerErr != nil {
			// Kept, not dropped: the message exists and the client can fetch
			// it, so removing it from the answer would be a second lie. What
			// it loses is everything the ordering is based on -- ancestry for
			// THREAD, subject and addresses for SORT -- hence the count.
			detached = append(detached, m.UID)
			byReason[unreadableReason(headerErr)]++
			lastReadErr = headerErr
		}
		out = append(out, one)
	}

	// Reported once per command with counts and one example, never per
	// message, and at WARN because the answer the client is about to receive
	// is wrong about those messages and nothing in it says so.
	if len(unreadable) > 0 || len(detached) > 0 {
		for reason, n := range byReason {
			metricUnreadable.WithLabelValues(command, reason).Add(float64(n))
		}
		example := append(append([]uint32(nil), unreadable...), detached...)[0]
		slog.Warn("imap: could not read some messages; the answer is wrong about them",
			"command", command,
			"user", s.userInfo.Username,
			"folder", s.folder.Name,
			// Excluded by a criterion that needed bytes nobody could read.
			"excluded", len(unreadable),
			// Answered, but with no headers behind them.
			"detached", len(detached),
			"records_scanned", len(msgs),
			"answered", len(out),
			"first_uid", example,
			"err", lastReadErr,
		)
	}
	return out, nil
}

// orderingMessage fills one message with what the command needs, from the
// cheapest source that has it.
//
// Three paths, in order of cost. The index answers ARRIVAL and SIZE with no
// message at all. The envelope cache answers subject, date and addresses
// without opening one -- ENVELOPE's addr-mailbox IS the local part RFC 5256
// sorts by, so nothing is re-parsed. Only References sends us to the file,
// and only THREAD wants it.
//
// A cache miss falls back to reading the header and stores the envelope back,
// which is what FETCH does: the miss is paid once, not once per command. It is
// deliberately not treated as "no data" -- an account whose cache is cold
// would otherwise sort by empty subjects and look like a mailbox of blank
// mail (#1448 made the same choice about a message that cannot be read).
func (s *session) orderingMessage(num uint32, m *mailbox.MessageMeta, raw []byte, needs orderingNeeds, envCache *msgcache.Handle) (imapthread.Message, error) {
	// The index answers ARRIVAL and SIZE, so a command asking only for those
	// never touches the message.
	if !needs.envelope && !needs.refs {
		return imapthread.Message{
			Num: num, Sent: m.InternalDate, Arrival: m.InternalDate,
			Size: int64(m.RFC822Size()),
		}, nil
	}
	out := imapthread.Message{
		Num: num, Sent: m.InternalDate, Arrival: m.InternalDate,
		Size: int64(m.RFC822Size()),
	}
	// Both halves must be cached for the message to stay unopened: an
	// envelope without References would thread the message by subject alone
	// and quietly put it in the wrong conversation, which is worse than
	// being slow.
	// One pass over the record, not one per field: each read walks the chain
	// and decodes everything in it, so asking twice doubled what a THREAD
	// spent on a large mailbox (#1461).
	// The head, not the whole envelope: ordering compares the date, the base
	// subject and the first mailbox of From/To/Cc, and decoding the six address
	// lists to reach them was 30.6% of every object a SORT (DATE) allocated on
	// a ten-thousand-message account (#1490).
	if needs.refs {
		if head, refs, ok := envCache.HeadAndReferences(m); ok {
			applyHead(&out, head)
			out.References = threadAncestry(refs, head.InReplyTo)
			return out, nil
		}
	} else if head, ok := envCache.Head(m); ok {
		applyHead(&out, head)
		return out, nil
	}
	// A miss is read, not skipped: an account with a cold cache would
	// otherwise sort by empty subjects and look like a mailbox of blank mail.
	// Extracted with the same function FETCH uses -- a second spelling of
	// ENVELOPE would be two answers about one message -- and stored, so the
	// next command over this mailbox takes the path above.
	if needs.refs {
		full, err := s.threadMessage(num, m, raw)
		if err != nil {
			return out, err
		}
		// The read is paid once: what it produced goes into the cache the
		// path above reads, so the next THREAD over this account opens
		// nothing. The cache is on disk, so "once" means once per account,
		// not once per process.
		if env, eerr := s.envelopeOf(m, raw); eerr == nil {
			envCache.StoreEnvelope(m, env)
			envCache.StoreReferences(m, full.References)
		}
		return full, nil
	}

	env, err := s.envelopeOf(m, raw)
	if err != nil {
		return out, err
	}
	envCache.StoreEnvelope(m, env)
	applyHead(&out, msgcache.HeadOf(env))
	return out, nil
}

// threadAncestry follows §4's rule about where ancestry comes from, over
// cached values: References when it names anything, otherwise the first
// In-Reply-To id -- the same order threadReferences applies to a live header.
func threadAncestry(refs []string, inReplyTo []string) []string {
	if len(refs) > 0 {
		return refs
	}
	if len(inReplyTo) > 0 {
		return []string{"<" + strings.Trim(inReplyTo[0], "<>") + ">"}
	}
	return nil
}

// envelopeOf parses the envelope from bytes already in hand, or by reading the
// message header.
func (s *session) envelopeOf(m *mailbox.MessageMeta, raw []byte) (*imaplib.Envelope, error) {
	if len(raw) > 0 {
		hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return nil, fmt.Errorf("imap/order: parse header of uid %d: %w", m.UID, err)
		}
		return imapserver.ExtractEnvelope(hdr), nil
	}
	if !mailbox.Readable(s.folderBox(), m) {
		return &imaplib.Envelope{}, nil
	}
	rc, err := s.fetchSelected(m)
	if err != nil {
		return nil, fmt.Errorf("imap/order: open uid %d: %w", m.UID, err)
	}
	defer rc.Close() //nolint:errcheck
	hdr, err := textproto.ReadHeader(bufio.NewReader(rc))
	if err != nil {
		return nil, fmt.Errorf("imap/order: read header of uid %d: %w", m.UID, err)
	}
	return imapserver.ExtractEnvelope(hdr), nil
}

// applyEnvelope fills the ordering fields ENVELOPE carries. Address.Mailbox is
// the addr-mailbox of RFC 5256 -- the local part, not the display name -- so
// the sort key comes straight out of the cache with nothing re-parsed.
func applyHead(out *imapthread.Message, head msgcache.Head) {
	out.Subject = head.Subject
	if !head.Date.IsZero() {
		out.Sent = head.Date.UTC()
	}
	out.MessageID = head.MessageID
	out.From = head.From
	out.To = head.To
	out.Cc = head.Cc
}

// threadMessage reads the headers threading needs. raw is whatever the match
// already read, so a search that had to open the message does not open it
// again.
func (s *session) threadMessage(num uint32, m *mailbox.MessageMeta, raw []byte) (imapthread.Message, error) {
	// ARRIVAL is the internal date, and DATE falls back to it when the Date
	// header is missing or unparsable (§2.2).
	out := imapthread.Message{
		Num:     num,
		Sent:    m.InternalDate,
		Arrival: m.InternalDate,
		Size:    int64(m.RFC822Size()),
	}
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
	// SORT keys, filled here because the header is already parsed: reading it
	// again per command would double the cost of the only expensive step.
	out.From = addrMailbox(hdr.Header.Get("From"))
	out.To = addrMailbox(hdr.Header.Get("To"))
	out.Cc = addrMailbox(hdr.Header.Get("Cc"))
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
	if !mailbox.Readable(s.folderBox(), m) {
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
