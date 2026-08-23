// Package threads persists which conversation each message belongs to.
//
// # Why a log and not a rewritten file
//
// The other per-user state files in this tree (subscriptions, special-use)
// are rewritten whole on every change, which is right for a set that changes
// a few times a day and is read far more often than written. This one is
// written on every delivery, and its size grows with the mailbox: rewriting
// it per message would make delivery cost O(account) per message, on the hot
// path, for the largest accounts first.
//
// So it is an append-only log with periodic compaction, the shape the mail
// index already uses for the same reason. A delivery appends a few short
// records under the account's thread lock; a reader loads the file once and
// folds it in memory.
//
// # Records
//
// TAB-delimited, LF-terminated, one record per line:
//
//	M<TAB><message-id><TAB><thread>    a Message-ID belongs to a thread
//	S<TAB><subject-key><TAB><thread>   a normalised subject was last seen on a thread
//	G<TAB><guid><TAB><thread>          one of OUR messages belongs to a thread
//	A<TAB><old-thread><TAB><thread>    old-thread was merged INTO thread
//
// An unknown record type is skipped rather than refused: a file written by a
// newer build must not make an older one refuse to thread at all, and the
// worst a skipped record costs is a join.
//
// # Merges never delete
//
// When a late message joins two conversations they merge, and the merge is an
// A record rather than a rewrite of every G record that named the old thread.
// A thread id therefore never dangles: it resolves, through as many aliases as
// it takes, to the thread that survives. That keeps a merge O(1) on the
// delivery path, and it keeps the id a client cached last week meaningful --
// it now names the merged conversation, which is what RFC 8621 expects a
// client to be told through Thread/changes.
//
// Alias chains are folded at compaction, so they do not grow without bound.
package threads

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the sidecar's name in the user's control directory.
const FileName = "threads"

// Record types on the wire.
const (
	recMessage = "M"
	recSubject = "S"
	recGUID    = "G"
	recAlias   = "A"
)

// State is the folded contents of the log: what the account knows about its
// conversations right now.
type State struct {
	byMessage map[string]string
	bySubject map[string]string
	byGUID    map[string]string
	aliasOf   map[string]string
}

// Empty is the state of an account whose sidecar does not exist yet -- which
// is every account until the migration step has run for it. It answers "no
// thread" to everything, so threading degrades to one-message-one-thread
// rather than to an error.
func Empty() *State {
	return &State{
		byMessage: map[string]string{},
		bySubject: map[string]string{},
		byGUID:    map[string]string{},
		aliasOf:   map[string]string{},
	}
}

// Load folds the log at path. A missing file is not an error: see Empty.
func Load(path string) (*State, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, fmt.Errorf("threads: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	st := Empty()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 3 {
			continue // truncated tail or a record shape we do not know
		}
		key, val := fields[1], fields[2]
		switch fields[0] {
		case recMessage:
			st.byMessage[key] = val
		case recSubject:
			st.bySubject[key] = val
		case recGUID:
			st.byGUID[key] = val
		case recAlias:
			st.aliasOf[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("threads: read %s: %w", path, err)
	}
	return st, nil
}

// resolve follows the alias chain to the thread that survives.
//
// Bounded by the number of aliases, and defensively by a step limit: a file
// corrupted into a cycle must not hang a delivery. A cycle answers with the id
// it started from, which is wrong but finite, and compaction breaks it.
func (s *State) resolve(id string) string {
	for i := 0; id != "" && i < len(s.aliasOf)+1; i++ {
		next, ok := s.aliasOf[id]
		if !ok {
			return id
		}
		id = next
	}
	return id
}

// ThreadOfMessage implements threading.Known.
func (s *State) ThreadOfMessage(messageID string) (string, bool) {
	id, ok := s.byMessage[messageID]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// ThreadOfSubject implements threading.Known.
func (s *State) ThreadOfSubject(key string) (string, bool) {
	id, ok := s.bySubject[key]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// ThreadOfGUID answers what Email.threadId and Thread/get report.
func (s *State) ThreadOfGUID(guid string) (string, bool) {
	id, ok := s.byGUID[guid]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// GUIDsOfThread lists the messages in a conversation, following aliases so a
// merged thread answers with everything that joined it.
func (s *State) GUIDsOfThread(threadID string) []string {
	want := s.resolve(threadID)
	var out []string
	for guid, id := range s.byGUID {
		if s.resolve(id) == want {
			out = append(out, guid)
		}
	}
	sort.Strings(out)
	return out
}

// Threads lists every conversation that still exists, merged ones excluded.
func (s *State) Threads() []string {
	seen := map[string]bool{}
	for _, id := range s.byGUID {
		seen[s.resolve(id)] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Placement is one message's assignment, ready to be appended.
type Placement struct {
	GUID       string
	MessageID  string
	SubjectKey string
	ThreadID   string
	// MergedFrom are threads this message folded into ThreadID.
	MergedFrom []string
}

// Append writes a placement to the log and applies it to s.
//
// The caller holds the account's thread lock: this is a read-modify-write in
// the sense that the placement was computed from s, and two deliveries
// computing from the same state would otherwise assign two thread ids to one
// conversation.
func Append(path string, s *State, p Placement) error {
	if p.ThreadID == "" {
		return fmt.Errorf("threads: placement for %s has no thread", p.GUID)
	}
	// Aliases first, and the asymmetry is the reason. A crash mid-write can
	// truncate the tail; whichever records are lost, the survivors must be the
	// safe half. An alias with no G record is a thread pointing at a thread --
	// harmless, it names a conversation that simply has one fewer message. A G
	// record with no alias is a merge that did not happen: the two halves of
	// one conversation stay apart, permanently. So the dangerous record goes
	// last.
	var b strings.Builder
	for _, old := range p.MergedFrom {
		writeRec(&b, recAlias, old, p.ThreadID)
	}
	if p.MessageID != "" {
		writeRec(&b, recMessage, p.MessageID, p.ThreadID)
	}
	if p.SubjectKey != "" {
		writeRec(&b, recSubject, p.SubjectKey, p.ThreadID)
	}
	writeRec(&b, recGUID, p.GUID, p.ThreadID)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("threads: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("threads: open %s: %w", path, err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close() //nolint:errcheck
		return fmt.Errorf("threads: append %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("threads: close %s: %w", path, err)
	}

	// Apply after the write succeeded, so an in-memory state never claims a
	// placement the file does not carry.
	s.byGUID[p.GUID] = p.ThreadID
	if p.MessageID != "" {
		s.byMessage[p.MessageID] = p.ThreadID
	}
	if p.SubjectKey != "" {
		s.bySubject[p.SubjectKey] = p.ThreadID
	}
	for _, old := range p.MergedFrom {
		s.aliasOf[old] = p.ThreadID
	}
	return nil
}

// writeRec appends one record. Values are flattened rather than escaped: a
// TAB inside a Message-ID or a subject key would shift the fields, and this
// file is read by position. Message-IDs do not legally contain one, and a
// subject key that does is a malformed header, not a record shape.
func writeRec(b *strings.Builder, typ, key, val string) {
	b.WriteString(typ)
	b.WriteByte('\t')
	b.WriteString(flatten(key))
	b.WriteByte('\t')
	b.WriteString(flatten(val))
	b.WriteByte('\n')
}

func flatten(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

// Compact rewrites the log as the smallest set of records that folds to the
// same state: aliases resolved away, one record per key.
//
// Aliases are the reason this is not optional. A merge is O(1) on the delivery
// path precisely because it appends an alias instead of rewriting membership,
// and a long-lived account accumulates them; folding them here is what keeps
// resolve() short and the file from growing without bound.
//
// tmp+rename, so a reader either sees the old file or the new one. The caller
// holds the account's thread lock, as for Append.
func Compact(path string, s *State) error {
	var recs []string
	for guid, id := range s.byGUID {
		recs = append(recs, record(recGUID, guid, s.resolve(id)))
	}
	for msg, id := range s.byMessage {
		recs = append(recs, record(recMessage, msg, s.resolve(id)))
	}
	for key, id := range s.bySubject {
		recs = append(recs, record(recSubject, key, s.resolve(id)))
	}
	// Sorted, so two processes compacting the same state produce byte-identical
	// files -- the same rule the subscriptions file follows.
	sort.Strings(recs)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("threads: mkdir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("threads: open %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, r := range recs {
		if _, err := w.WriteString(r); err != nil {
			f.Close()      //nolint:errcheck
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("threads: write %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: rename %s: %w", tmp, err)
	}
	// The file now carries resolved values, so the live state must too --
	// BOTH halves. Dropping the aliases while leaving the maps pointing at
	// swallowed threads is worse than doing nothing: the next delivery
	// computed from this state would place a reply into a thread that no
	// longer exists, splitting a conversation that had already been merged.
	// That is the permanent wrong answer this whole design exists to avoid,
	// and it would be introduced by the tidying step.
	for k, id := range s.byGUID {
		s.byGUID[k] = s.resolve(id)
	}
	for k, id := range s.byMessage {
		s.byMessage[k] = s.resolve(id)
	}
	for k, id := range s.bySubject {
		s.bySubject[k] = s.resolve(id)
	}
	for old := range s.aliasOf {
		delete(s.aliasOf, old)
	}
	return nil
}

func record(typ, key, val string) string {
	var b strings.Builder
	writeRec(&b, typ, key, val)
	return b.String()
}
