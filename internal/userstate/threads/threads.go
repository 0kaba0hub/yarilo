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
	"sync"
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
//
// # Why the lock is here and not around it
//
// Writers are serialised ACROSS processes by the account's thread lock, which
// is where cross-process consistency belongs. What remains is a race inside
// one process: a reader (Thread/get) walking these maps while a delivery
// applies a placement to them.
//
// The distributed lock is the wrong currency for that -- a reader would pay a
// network round trip to the lock service per request and queue behind LMTP,
// making a JMAP read as slow as the writes it has nothing to do with. A
// snapshot is the other wrong answer: copying the account per request is the
// O(account) cost the fold cache exists to avoid, moved onto the read path.
//
// So: an RWMutex, held for nanoseconds. Readers take it shared, the memory
// half of Append takes it exclusively -- and because Append applies to memory
// only after the file write succeeded, that block is indivisible: a reader
// never sees a message placed without the alias that placed it.
type State struct {
	mu        sync.RWMutex
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byMessage[messageID]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// ThreadOfSubject implements threading.Known.
func (s *State) ThreadOfSubject(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.bySubject[key]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// ThreadOfGUID answers what Email.threadId and Thread/get report.
func (s *State) ThreadOfGUID(guid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.threadOfGUID(guid)
}

func (s *State) threadOfGUID(guid string) (string, bool) {
	id, ok := s.byGUID[guid]
	if !ok {
		return "", false
	}
	return s.resolve(id), true
}

// GUIDsOfThread lists the messages in a conversation, following aliases so a
// merged thread answers with everything that joined it.
func (s *State) GUIDsOfThread(threadID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guidsOfThread(threadID)
}

func (s *State) guidsOfThread(threadID string) []string {
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.threads()
}

func (s *State) threads() []string {
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
	// No fsync here, and that is a decision rather than an omission:
	//
	//  1. it does not remove the window, only narrows it -- an OS crash tears
	//     between synced writes as readily as within one;
	//  2. the real protection is the record ORDER below: whatever survives a
	//     truncated tail is the harmless half;
	//  3. this sidecar is DERIVED state. The migration step that builds it for
	//     existing accounts is also the tool that rebuilds it, so a lost tail
	//     is recoverable -- unlike mail, which is not.
	//
	// Paying an fsync per delivery for a rebuildable derived structure is the
	// wrong trade. If a field case ever shows tails going missing
	// systematically -- something other than a crash -- this comes back with a
	// number behind it.
	//
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
	// placement the file does not carry -- and all at once, so a reader sees
	// the placement with its alias or neither.
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Read runs fn while holding the state still.
//
// One request, one state. A reader that took the lock per lookup could answer
// from two states -- the thread of a message read before a merge, its member
// list read after -- and the client would receive a conversation that never
// existed. The hold is nanoseconds: the write side is a few map assignments,
// already off the file I/O.
func (s *State) Read(fn func(View)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(View{s: s})
}

// View is the account's conversations, held still for the duration of one
// Read. Its methods do not lock: the lock is the Read around them.
type View struct{ s *State }

// ThreadOf reports the conversation a message belongs to.
func (v View) ThreadOf(guid string) (string, bool) { return v.s.threadOfGUID(guid) }

// Members lists the messages of a conversation, following merges so an id a
// client cached still answers with the conversation it became.
func (v View) Members(threadID string) []string { return v.s.guidsOfThread(threadID) }

// Threads lists every conversation the account has.
func (v View) Threads() []string { return v.s.threads() }
