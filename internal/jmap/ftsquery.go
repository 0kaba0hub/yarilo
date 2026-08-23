package jmap

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FTS wires full-text search into Email/query. A nil Client leaves the
// index-only evaluator, which refuses text conditions by name rather than
// answering them wrongly.
type FTS struct {
	Client ftsproto.Client
	// Chain must be the same set the yarilo-fts service indexes with, for the
	// same reason as on the IMAP side: query expansion has to cover every
	// language indexing could have picked.
	Chain *language.MultiChain
	// MaxConns mirrors fts_max_conns. One request takes at most half of it, so
	// a second request always has connections left (see requestConcurrency).
	MaxConns int
	// MaxFolders is jmap_max_query_folders.
	MaxFolders int
	// AddMissing and Timeout mirror fts_search_add_missing and fts_timeout:
	// whether a lagging folder is queued for priority indexing, and how long a
	// query waits for it before answering "retry".
	AddMissing string
	Timeout    time.Duration
}

// errIndexLagging and errPoolBusy are transient: the client should retry rather
// than conclude the mail is not there, so they answer serverUnavailable while
// an outright lookup failure answers serverFail.
var (
	errIndexLagging = errors.New("jmap: full-text index is catching up")
	errPoolBusy     = errors.New("jmap: no free connection to the full-text service")
)

// errFolderWithoutGUID is a server condition, not a bad request: the client
// named nothing wrong and cannot narrow inMailbox to avoid a folder whose
// identity it cannot see.
type errFolderWithoutGUID struct{ folder string }

func (e *errFolderWithoutGUID) Error() string {
	return fmt.Sprintf("jmap: folder %q has no GUID, so it cannot be searched", e.folder)
}

// requestConcurrency is how many folders one request looks up at once. Half the
// pool, so two concurrent requests cannot starve each other into the transient
// refusal above -- the pool bounds what the service takes, this bounds what one
// request may take of it.
func (f *FTS) requestConcurrency() int {
	if f.MaxConns/2 > 1 {
		return f.MaxConns / 2
	}
	return 1
}

// ftsEvaluator answers the conditions the index cannot, by asking yarilo-fts
// per folder. It composes with indexEvaluator rather than replacing it: every
// non-text condition is still answered from message metadata.
type ftsEvaluator struct {
	indexEvaluator
	fts  *FTS
	user string
	box  mailbox.UserMailbox

	// deadline bounds the whole request's waiting, not each folder's: with a
	// fan-out the size of the ceiling, a per-folder budget would multiply into
	// minutes with the client holding the line and half the pool held.
	deadline time.Time

	mu sync.Mutex
	// byFolder is what prepare resolved, per folder id.
	byFolder map[uint64]*folderMatches
}

// startRequest opens the shared waiting budget for one query.
func (e *ftsEvaluator) startRequest() {
	timeout := e.fts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	e.deadline = time.Now().Add(timeout)
}

// folderMatches is one folder's lookup outcome. verify holds the UIDs the
// engine could only over-approximate; they are confirmed against the message
// before they reach a response, which is the invariant IMAP holds too.
type folderMatches struct {
	covered map[uint32]bool
	verify  map[uint32]bool
	// terms is what a verified UID is checked against.
	terms []verifyTerm
}

// verifyTerm is one condition in the form the confirmation uses: where the
// string may appear, and the string itself. A condition matches when ANY of
// its places contains it -- "text" spans the address headers, the subject and
// the body (RFC 8621 §4.4.1), so confirming it against the body alone would
// drop a message whose only hit is in the subject.
type verifyTerm struct {
	headers []string
	body    bool
	want    string
}

// textHeaders are the header fields the "text" condition covers besides the
// body, per RFC 8621 §4.4.1.
var textHeaders = []string{"from", "to", "cc", "bcc", "subject"}

func (s *Server) newFTSEvaluator(h *userHandle) *ftsEvaluator {
	return &ftsEvaluator{
		fts:      s.opts.FTS,
		user:     h.info.Username,
		box:      h.box,
		byFolder: map[uint64]*folderMatches{},
	}
}

// unsupported: nothing. Every text condition is answerable now, which is the
// whole point of this evaluator.
func (e *ftsEvaluator) unsupported(*jmapcore.EmailFilter) []string { return nil }

// prepare looks one folder up. It runs once per folder, and the query runs
// several of them at once (see prepareScope).
func (e *ftsEvaluator) prepare(ctx context.Context, h *userHandle, sf scopeFolder, f *jmapcore.EmailFilter) error {
	terms := f.TextConditions()
	if len(terms) == 0 {
		return nil
	}
	// Nobody is waiting any more, or a sibling folder has already decided the
	// answer: neither is worth a lock and a round trip.
	if err := ctx.Err(); err != nil {
		return err
	}
	if sf.guid == "" {
		return &errFolderWithoutGUID{folder: sf.name}
	}
	if err := e.catchUp(ctx, h, sf); err != nil {
		return err
	}
	query, verify, impossible := e.buildQuery(f)
	if impossible {
		// A condition whose every token expanded to nothing can never match:
		// stopwords were never indexed. That is a definite answer, so it does
		// not consult the service at all.
		//
		// Said out loud, because from outside it is indistinguishable from a
		// query that ran and found nothing: no FTS line, no hits, no error
		// (#1279). A term that expands to nothing is usually a stopword, a
		// token past the tokenizer's length cap, or a chain configured
		// differently from the one that indexed.
		slog.Info("jmap: query expanded to nothing, answering without consulting fts",
			"user", e.user, "folder", sf.name, "conditions", terms)
		e.store(sf.id, &folderMatches{covered: map[uint32]bool{}})
		return nil
	}

	res, err := e.fts.Client.Lookup(e.user, fts.MailboxRef{
		GUID: sf.guid, Name: sf.name, UIDValidity: sf.uidValidity,
	}, query)
	if err != nil {
		if errors.Is(err, ftsproto.ErrPoolExhausted) {
			return errPoolBusy
		}
		return fmt.Errorf("jmap: fts lookup %q: %w", sf.name, err)
	}

	fm := &folderMatches{
		covered: make(map[uint32]bool, len(res.Definite)+len(res.Maybe)),
		verify:  make(map[uint32]bool, len(res.Maybe)),
		terms:   verify,
	}
	for _, uid := range res.Definite {
		fm.covered[uid] = true
	}
	for _, uid := range res.Maybe {
		fm.covered[uid] = true
		fm.verify[uid] = true
	}
	slog.Debug("jmap: fts candidates", "user", e.user, "folder", sf.name,
		"definite", len(res.Definite), "maybe", len(res.Maybe))
	e.store(sf.id, fm)
	return nil
}

// catchUp waits for a lagging folder rather than searching a stale index. IMAP
// falls back to the exact scan at this point; Email/query has no scan, so the
// only honest ending is "retry" -- a result computed from a half-indexed folder
// would be missing mail and say nothing about it.
func (e *ftsEvaluator) catchUp(ctx context.Context, h *userHandle, sf scopeFolder) error {
	metas, err := mailbox.ReadMessages(h.idx, sf.id, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(metas) == 0 {
		return nil
	}
	maxUID := metas[len(metas)-1].UID
	mbox := fts.MailboxRef{GUID: sf.guid, Name: sf.name, UIDValidity: sf.uidValidity}

	last, _, err := e.fts.Client.Status(e.user, mbox)
	if err == nil && last >= maxUID {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jmap: fts status %q: %w", sf.name, err)
	}
	// Queuing the folder needs the knob; noticing that the index is behind does
	// not. Without it there is nothing to wait for, so the query says so at
	// once instead of searching a folder it knows is half-indexed.
	if e.fts.AddMissing == "" {
		slog.Warn("jmap: fts index behind and fts_search_add_missing is unset",
			"user", e.user, "folder", sf.name, "indexed", last, "want", maxUID)
		return errIndexLagging
	}
	if perr := e.fts.Client.Prepend(e.user, mbox, maxUID); perr != nil {
		return fmt.Errorf("jmap: fts prepend %q: %w", sf.name, perr)
	}
	// Give up early when the checkpoint stops moving: a wedged index would
	// otherwise hold the request for the whole budget, which the client's own
	// deadline is usually shorter than -- so it hangs up and the work
	// continues for nobody. IMAP grew the same early exit in #629.
	const stallPolls = 8 // 8 x 250ms = ~2s of no movement
	best, stalls := last, 0
	for time.Now().Before(e.deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		cur, _, serr := e.fts.Client.Status(e.user, mbox)
		if serr != nil {
			return fmt.Errorf("jmap: fts status %q: %w", sf.name, serr)
		}
		if cur >= maxUID {
			return nil
		}
		if cur > best {
			best, stalls = cur, 0
			continue
		}
		if stalls++; stalls >= stallPolls {
			slog.Warn("jmap: fts index is not advancing, not waiting out the budget",
				"user", e.user, "folder", sf.name, "indexed", cur, "want", maxUID)
			return errIndexLagging
		}
	}
	slog.Warn("jmap: fts index still behind, asking the client to retry",
		"user", e.user, "folder", sf.name, "indexed", last, "want", maxUID)
	return errIndexLagging
}

func (e *ftsEvaluator) store(id uint64, fm *folderMatches) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.byFolder[id] = fm
}

func (e *ftsEvaluator) matches(id uint64) (*folderMatches, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fm, ok := e.byFolder[id]
	return fm, ok
}

// match answers a message: the index conditions as before, plus membership in
// what the lookup returned. A UID the engine could only guess at is confirmed
// against the message itself first.
func (e *ftsEvaluator) match(m *mailbox.MessageMeta, sf scopeFolder, scope *queryScope, f *jmapcore.EmailFilter) bool {
	if !e.indexEvaluator.match(m, sf, scope, f) {
		return false
	}
	fm, ok := e.matches(sf.id)
	if !ok {
		return true // no text condition in this filter
	}
	if !fm.covered[m.UID] {
		return false
	}
	if !fm.verify[m.UID] {
		return true
	}
	return e.confirm(sf, m, fm.terms)
}

// confirm re-checks an over-approximated hit against the message. It uses the
// same substring semantics IMAP SEARCH does, so both surfaces agree on what a
// match is; a message that cannot be read does not match, since claiming a hit
// nobody can verify is the failure this exists to prevent.
func (e *ftsEvaluator) confirm(sf scopeFolder, m *mailbox.MessageMeta, terms []verifyTerm) bool {
	if len(terms) == 0 {
		return true
	}
	hdr, body, err := e.readMessage(sf, m)
	if err != nil {
		slog.Warn("jmap: cannot verify a full-text candidate", "folder", sf.name, "uid", m.UID, "err", err)
		return false
	}
	for _, t := range terms {
		if !t.matches(hdr, body) {
			return false
		}
	}
	return true
}

// matches reports whether the condition is satisfied anywhere it may appear.
// Header values are decoded first: the index matched the decoded form, and the
// client sent what it reads, so comparing against a raw encoded word would
// reject the hit the engine legitimately proposed.
func (t verifyTerm) matches(hdr message.Header, body string) bool {
	want := strings.ToLower(t.want)
	if t.body && strings.Contains(strings.ToLower(body), want) {
		return true
	}
	for _, name := range t.headers {
		if strings.Contains(strings.ToLower(decodeWord(hdr.Get(name))), want) {
			return true
		}
	}
	return false
}

// readMessage returns the header and every text part's decoded body joined,
// which is what a condition is confirmed against: a hit may be anywhere the
// index looked.
func (e *ftsEvaluator) readMessage(sf scopeFolder, m *mailbox.MessageMeta) (message.Header, string, error) {
	hdr, parts, err := e.readParts(sf, m)
	if err != nil {
		return message.Header{}, "", err
	}
	var text strings.Builder
	for _, p := range parts {
		if strings.HasPrefix(p.mediaType, "text/") {
			text.WriteString(p.body)
			text.WriteString("\n")
		}
	}
	return hdr, text.String(), nil
}

func (e *ftsEvaluator) readParts(sf scopeFolder, m *mailbox.MessageMeta) (message.Header, []walkedPart, error) {
	rc, err := e.box.Fetch(sf.name, m.Filename, m.AltTier)
	if err != nil {
		return message.Header{}, nil, err
	}
	defer rc.Close() //nolint:errcheck
	entity, err := message.Read(rc)
	if err != nil && entity == nil {
		return message.Header{}, nil, err
	}
	return entity.Header, collectParts(entity, ""), nil
}

// buildQuery converts the filter's text conditions into the engine query, and
// returns what a verified hit must be re-checked against.
//
// Header fields go through the no-stemming data chain and body text through the
// configured language chain -- the same split indexing uses, since a header is
// not language text.
func (e *ftsEvaluator) buildQuery(f *jmapcore.EmailFilter) (fts.Query, []verifyTerm, bool) {
	var terms []fts.Term
	var verify []verifyTerm
	impossible := false

	add := func(exp expander, field fts.FieldKind, hdrName, value string, where verifyTerm) {
		if value == "" {
			return
		}
		words := exp.ExpandSearch(value)
		if len(words) == 0 {
			impossible = true
			return
		}
		t := fts.Term{Field: field, HdrName: hdrName, Words: words}
		if strings.ContainsRune(strings.TrimSpace(value), ' ') {
			t.Phrase = value
		}
		terms = append(terms, t)
		where.want = value
		verify = append(verify, where)
	}

	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	add(e.fts.Chain, fts.FieldText, "", str(f.Text), verifyTerm{headers: textHeaders, body: true})
	add(e.fts.Chain, fts.FieldBody, "", str(f.Body), verifyTerm{body: true})
	for name, value := range map[string]string{
		"subject": str(f.Subject), "from": str(f.From), "to": str(f.To),
		"cc": str(f.Cc), "bcc": str(f.Bcc),
	} {
		add(headerDataChain, fts.FieldHeader, name, value, verifyTerm{headers: []string{name}})
	}
	if len(f.Header) > 0 {
		name := strings.ToLower(f.Header[0])
		if len(f.Header) == 1 {
			// Presence only: the header exists, whatever it holds.
			terms = append(terms, fts.Term{Field: fts.FieldHeader, HdrName: name})
		} else {
			add(headerDataChain, fts.FieldHeader, name, f.Header[1], verifyTerm{headers: []string{name}})
		}
	}
	return fts.Query{Terms: terms, AndTerms: true}, verify, impossible
}

// expander is whatever expands a query string into engine words.
type expander interface {
	ExpandSearch(query string) []fts.Word
}

// headerDataChain is the no-stemming chain header fields are matched through,
// mirroring how buildmail indexes them.
var headerDataChain = mustHeaderDataChain()

func mustHeaderDataChain() *language.Chain {
	c, err := language.NewDataChain()
	if err != nil {
		panic(fmt.Sprintf("jmap: header data chain: %v", err))
	}
	return c
}

// ftsMailboxGUID renders a folder GUID the way the wire expects it. An unset
// GUID stays empty rather than becoming a string of zeroes, so the caller can
// tell the folder apart from one that has an identity.
func ftsMailboxGUID(guid [16]byte) string {
	if guid == ([16]byte{}) {
		return ""
	}
	return hex.EncodeToString(guid[:])
}

// checkQueryFolders refuses a query whose full-text fan-out would exceed
// jmap_max_query_folders. The refusal names both numbers because the client
// can act on it -- narrowing inMailbox is exactly what it is being asked to
// do -- and because a truncated fan-out would answer with a subset of the
// mail while looking like the whole of it.
func (s *Server) checkQueryFolders(scope *queryScope, f *jmapcore.EmailFilter) *jmapcore.MethodError {
	if s.opts.FTS == nil || len(f.TextConditions()) == 0 {
		return nil
	}
	max := s.opts.FTS.MaxFolders
	if max <= 0 || len(scope.folders) <= max {
		return nil
	}
	return &jmapcore.MethodError{
		Type: jmapcore.ErrInvalidArguments,
		Description: fmt.Sprintf(
			"a full-text filter over %d mailboxes exceeds the server limit of %d; narrow it with inMailbox",
			len(scope.folders), max),
	}
}

// prepareScope resolves every folder before any message is read, so the
// lookups overlap instead of running one per pass of the match loop.
//
// Concurrency is bounded here rather than left to the pool: an exhausted pool
// answers with an error after its wait, so more goroutines than connections
// would turn load into refusals.
func (s *Server) prepareScope(ctx context.Context, h *userHandle, eval filterEvaluator, scope *queryScope, f *jmapcore.EmailFilter) *jmapcore.MethodError {
	width := 1
	if s.opts.FTS != nil {
		width = s.opts.FTS.requestConcurrency()
	}
	if fe, ok := eval.(*ftsEvaluator); ok {
		fe.startRequest()
	}
	// One folder's refusal is the whole query's answer, so the rest stop
	// rather than each taking a lock and a round trip for a result that will
	// be discarded. Cancelling also carries the caller's own deadline down:
	// without it the backend kept working for eleven seconds after the client
	// had hung up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, width)
	errs := make([]error, len(scope.folders))
	var wg sync.WaitGroup
	for i, sf := range scope.folders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := eval.prepare(ctx, h, sf, f); err != nil {
				errs[i] = err
				cancel()
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil || errors.Is(err, context.Canceled) {
			// A folder that stopped because a sibling had already decided the
			// answer is not itself a finding.
			continue
		}
		return queryPrepareError(scope.folders[i].name, err)
	}
	// Any cancellation, including a plain one. Reaching here means no folder
	// reported a real failure, so a cancelled context is the caller's own --
	// and answering it with nil would leave every folder without a stored
	// lookup, which match reads as "this filter has no text condition" and
	// passes every message. A search would answer with the whole mailbox.
	if err := ctx.Err(); err != nil {
		return &jmapcore.MethodError{Type: jmapcore.ErrServerUnavailable, Description: err.Error()}
	}
	return nil
}

// queryPrepareError maps a failure to the type that tells the client what to
// do with it: retry later, fix the request, or neither.
func queryPrepareError(folder string, err error) *jmapcore.MethodError {
	var noGUID *errFolderWithoutGUID
	switch {
	case errors.Is(err, errPoolBusy), errors.Is(err, errIndexLagging):
		// Transient: the service is alive and the condition passes on its own,
		// so serverFail would let a client show an empty result as final.
		slog.Warn("jmap: full-text search busy", "folder", folder, "err", err)
		return &jmapcore.MethodError{Type: jmapcore.ErrServerUnavailable, Description: err.Error()}
	case errors.As(err, &noGUID):
		// Not the client's doing and not fixable from the outside: it cannot
		// see which folder lacks an identity, let alone exclude it.
		slog.Error("jmap: folder cannot be searched", "folder", folder, "err", err)
		return &jmapcore.MethodError{Type: jmapcore.ErrServerFail, Description: err.Error()}
	case errors.Is(err, ftsproto.ErrUnavailable):
		// The FTS service could not reach a dependency of its own. Since
		// #1409 that crosses the wire, so this arm can tell it apart from an
		// index that is broken -- which it could not before, and answered
		// both as defects.
		slog.Warn("jmap: full-text search unavailable", "folder", folder, "err", err)
		return &jmapcore.MethodError{
			Type:        jmapcore.ErrServerUnavailable,
			Description: "full-text search is temporarily unavailable, try again",
		}
	default:
		slog.Warn("jmap: Email/query prepare failed", "folder", folder, "err", err)
		return &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}
}
