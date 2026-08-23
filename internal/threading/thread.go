package threading

// Message is what threading needs from a delivered message: its own identity,
// what it claims to answer, and its subject.
type Message struct {
	MessageID  string
	InReplyTo  []string
	References []string
	Subject    string
}

// Known is the account's threading state as far as the caller can see it: what
// thread a message id already belongs to, and what thread a subject key was
// last seen on.
//
// An interface rather than a map, because the caller owns the storage and this
// package must not: the sidecar's shape is its own decision, and the algorithm
// has no business depending on it.
type Known interface {
	// ThreadOfMessage returns the thread a known message id belongs to.
	ThreadOfMessage(messageID string) (threadID string, ok bool)
	// ThreadOfSubject returns the thread a normalised subject key was last
	// joined to. A caller that does not want subject joining at all returns
	// false here, and threading degrades to identity only.
	ThreadOfSubject(key string) (threadID string, ok bool)
}

// Result is the placement of one message.
type Result struct {
	// ThreadID is the thread the message belongs to. Empty means the caller
	// must mint a new one -- minting is the caller's, because a thread id has
	// to be unique in a namespace this package cannot see.
	ThreadID string
	// MergedFrom lists threads that this message joined together and that no
	// longer exist as separate conversations. Their messages move to ThreadID,
	// and the change is reported to clients (RFC 8621 allows threadId to
	// change, which is what Thread/changes is for).
	MergedFrom []string
	// SubjectKey is the normalised subject, so the caller can record it
	// against the resulting thread. Empty when the subject cannot join.
	SubjectKey string
}

// Resolve places a message.
//
// Identity first: every id the message claims to answer is looked up, and each
// hit is a thread this message belongs to. More than one hit means a message
// that arrived late and joins conversations that had grown apart -- they are
// MERGED rather than left separate. A split thread is a permanent wrong answer
// in the data; a merge is a one-off change the protocol carries by design.
//
// Subject is the fallback, and only when identity said nothing at all. Using it
// to merge threads that identity had already separated is how unrelated mail
// with the same subject ends up in one conversation.
func Resolve(msg Message, known Known) Result {
	var res Result
	res.SubjectKey = NormalizeSubject(msg.Subject)

	seen := make(map[string]bool)
	var threads []string
	for _, ref := range refsOf(msg) {
		id, ok := known.ThreadOfMessage(ref)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		threads = append(threads, id)
	}

	if len(threads) > 0 {
		// The first is kept and the rest fold into it. Which one survives is
		// arbitrary but must be deterministic: two processes threading the
		// same delivery order must reach the same answer.
		res.ThreadID = threads[0]
		res.MergedFrom = threads[1:]
		return res
	}

	if res.SubjectKey != "" {
		if id, ok := known.ThreadOfSubject(res.SubjectKey); ok {
			res.ThreadID = id
			return res
		}
	}
	// Nothing to join: the caller mints a thread for it.
	return res
}

// refsOf returns the ids this message claims to answer, In-Reply-To first.
//
// In-Reply-To is the direct parent and the most trustworthy; References is the
// chain, and clients truncate it, so a hit anywhere in it still places the
// message. Its own Message-ID is not among them -- a message does not answer
// itself, and including it would join two messages that merely share a
// malformed id.
func refsOf(msg Message) []string {
	out := make([]string, 0, len(msg.InReplyTo)+len(msg.References))
	for _, id := range msg.InReplyTo {
		if id != "" && id != msg.MessageID {
			out = append(out, id)
		}
	}
	for _, id := range msg.References {
		if id != "" && id != msg.MessageID {
			out = append(out, id)
		}
	}
	return out
}
