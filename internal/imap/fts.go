package imap

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// headerDataChain expands HEADER search values through the same "data" chain
// buildmail indexes header values with: normalization only, no stemming, no
// stopwords, independent of the configured language(s). A stemmed query variant
// against an unstemmed indexed header token yields false-positive wildcard
// matches (e.g. "running" -> "run*" matching an unrelated "runway").
var headerDataChain = mustHeaderDataChain()

// expander is satisfied by both *language.MultiChain (Body/Text, full language
// stemming) and *language.Chain (Header, the no-stemming data chain);
// buildFTSQuery picks whichever fits the field.
type expander interface {
	ExpandSearch(query string) []fts.Word
}

func mustHeaderDataChain() *language.Chain {
	c, err := language.NewDataChain()
	if err != nil {
		// "lowercase" is a static, language-independent filter: cannot fail at
		// runtime, only if the filter chain is broken at compile time.
		panic(fmt.Sprintf("imap: header data chain: %v", err))
	}
	return c
}

// FTSOptions wires full-text search into IMAP sessions. Nil Client disables
// FTS entirely — SEARCH keeps the sequential scan.
type FTSOptions struct {
	Client ftsproto.Client
	// Chain must match the yarilo-fts service's configured language SET
	// (order-independent) so query expansion covers exactly the languages
	// indexing could have picked. Query expansion is deliberately asymmetric
	// from indexing: it fans out through every configured language, OR'd
	// together, since a query doesn't know which single language a given
	// message was auto-detected as.
	Chain *language.MultiChain
	// AddMissing / ReadFallback / Timeout / Strict — see https://doc.yarilomail.org/FTS §11.
	AddMissing   string
	ReadFallback bool
	Timeout      time.Duration
	Strict       bool
	// FirstIndexGrace bounds the wait for a mailbox with nothing indexed yet.
	// See the cold-start comment in ftsCatchUp: the lag heuristic needs
	// movement to judge, and a first index provides none (#1379).
	FirstIndexGrace time.Duration
	// Autoindex triggers INDEX on delivered events; MaxRecent is the
	// autoindex throttle forwarded to the service.
	Autoindex bool
	MaxRecent int
	// SearchEnabled gates SEARCH only (fts_search): false degrades every SEARCH
	// to the sequential scan while indexing/autoindex/write-through keep
	// running. An incident-response knob for "the FTS engine is misbehaving,
	// stop querying it, but don't let the index go stale." Distinct from
	// fts.enabled (all-or-nothing, including indexing) at the config layer.
	SearchEnabled bool
}

func (o FTSOptions) enabled() bool { return o.Client != nil && o.Chain != nil && o.SearchEnabled }

// folderGUIDWarned dedupes the missing-GUID warning to one per folder per
// process; the condition is a property of the folder, not of the message.
var folderGUIDWarned sync.Map

func warnFolderWithoutGUID(user, folder string) {
	if _, seen := folderGUIDWarned.LoadOrStore(user+"\x00"+folder, struct{}{}); seen {
		return
	}
	slog.Warn("imap: folder has no GUID; full-text indexing skipped for it",
		"user", user, "folder", folder)
}

// ftsMailboxRef maps the selected folder to the wire mailbox identity.
func ftsMailboxRef(f *mailbox.Folder) fts.MailboxRef {
	return fts.MailboxRef{
		Name:        f.Name,
		GUID:        hex.EncodeToString(f.GUID[:]),
		UIDValidity: f.UIDValidity,
	}
}

// ftsFilter is the per-SEARCH outcome of an FTS lookup: which UIDs remain
// candidates, which need raw re-verification, and the criteria stripped of
// the keys FTS already answered.
type ftsFilter struct {
	covered  map[uint32]bool
	verify   map[uint32]bool
	stripped *imaplib.SearchCriteria
	// strippedNeedsBody: the stripped criteria still require the raw
	// message (sent-date checks).
	strippedNeedsBody bool
	// scores holds the engine's native (unnormalized) ranking weight per UID,
	// when the engine populated fts.Result.Scores (flatcurve does via the
	// Xapian MSet weight). Nil when the FTS constraint expanded to nothing
	// indexed (pure-stopword query) or the engine returned none — callers omit
	// RELEVANCY rather than fabricate a score. Normalization to the RFC
	// 4731/6203 wire range (min-max, 1-100) happens once per SEARCH, after the
	// final matched set is known — see relevancyScores in server.go.
	scores map[uint32]float64
}

// prepareFTSSearch runs the FTS half of a SEARCH. Returns (nil, nil) when
// FTS is not engaged (disabled, no body criteria, nested body criteria, or
// fallback after an FTS failure with ReadFallback on); returns an IMAP error
// when FTS failed and fallback is off.
func (s *session) prepareFTSSearch(criteria *imaplib.SearchCriteria, msgs []*mailbox.MessageMeta) (*ftsFilter, *imaplib.Error) {
	o := s.srv.opts.FTS
	if !o.enabled() || s.userInfo == nil {
		return nil, nil
	}
	if len(criteria.Body) == 0 && len(criteria.Text) == 0 && len(criteria.Header) == 0 {
		return nil, nil
	}
	// Body criteria nested under NOT/OR keep the exact scan semantics.
	if searchNeedsBodyRecurse(criteria.Not, criteria.Or) {
		return nil, nil
	}

	query, stripped, strippedNeedsBody, impossible := s.buildFTSQuery(criteria)
	if impossible {
		// At least one Body/Text/Header criterion expanded to nothing (pure
		// stopwords): that criterion can never match, since stopwords were
		// never indexed, so the whole ANDed query is unmatchable regardless of
		// any other criteria that DID expand to real terms. This is a definite
		// answer, not an index-unavailable condition, so it bypasses
		// ftsCatchUp/fallback entirely, same as the Lookup path below.
		return &ftsFilter{
			covered:           map[uint32]bool{},
			verify:            map[uint32]bool{},
			stripped:          stripped,
			strippedNeedsBody: strippedNeedsBody,
		}, nil
	}

	mbox := ftsMailboxRef(s.folder)
	user := s.userInfo.Username

	fallback, imapErr := s.ftsCatchUp(user, mbox, msgs)
	if imapErr != nil {
		return nil, imapErr
	}
	if fallback {
		return nil, nil
	}

	res, err := o.Client.Lookup(user, mbox, query)
	if err != nil {
		slog.Warn("imap: fts lookup failed", "user", user, "folder", mbox.Name, "err", err)
		if o.ReadFallback {
			return nil, nil
		}
		// A dependency of the FTS service being restarted is a wait, and the
		// code has to say so as well as the text: a bare NO reads the same as
		// an FTS that is broken for good, so a client stops asking for
		// something that works again in seconds (#1409).
		if errors.Is(err, ftsproto.ErrUnavailable) {
			return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo,
				Code: imaplib.ResponseCodeUnavailable,
				Text: "Full-text search is temporarily unavailable, try again"}
		}
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo,
			Text: "Full-text search unavailable"}
	}

	f := &ftsFilter{
		covered:           make(map[uint32]bool, len(res.Definite)+len(res.Maybe)),
		verify:            make(map[uint32]bool, len(res.Maybe)),
		stripped:          stripped,
		strippedNeedsBody: strippedNeedsBody,
	}
	for _, uid := range res.Definite {
		f.covered[uid] = true
		if o.Strict {
			f.verify[uid] = true
		}
	}
	for _, uid := range res.Maybe {
		f.covered[uid] = true
		f.verify[uid] = true
	}
	if len(res.Scores) > 0 {
		f.scores = make(map[uint32]float64, len(res.Scores))
		for _, sc := range res.Scores {
			f.scores[sc.UID] = sc.Value
		}
	}
	// Visibility diagnostic: how many candidates the FTS index returned, so a
	// "search finds nothing" case shows whether FTS had no hits (0 → likely not
	// indexed) vs. hits that later failed re-verify. Counts only, never the
	// query terms (private mail content).
	slog.Debug("imap: fts search candidates",
		"user", user, "folder", mbox.Name,
		"definite", len(res.Definite), "maybe", len(res.Maybe))
	return f, nil
}

// ftsCatchUp implements on-demand indexing at SEARCH: when the index is
// behind the mailbox, a priority PREPEND is queued and the session polls
// until the checkpoint catches up or the timeout expires. fallback=true
// means the caller must run the sequential scan instead.
func (s *session) ftsCatchUp(user string, mbox fts.MailboxRef, msgs []*mailbox.MessageMeta) (fallback bool, imapErr *imaplib.Error) {
	o := s.srv.opts.FTS
	if o.AddMissing == "" || len(msgs) == 0 {
		return false, nil
	}
	maxUID := msgs[len(msgs)-1].UID
	last, _, err := o.Client.Status(user, mbox)
	if err == nil && last >= maxUID {
		return false, nil
	}
	// Kept across the branches below: whichever call failed, the answer at the
	// end has to say whether it failed because the service could not reach a
	// dependency.
	var serviceErr error
	if err != nil {
		serviceErr = err
		slog.Warn("imap: fts status failed", "user", user, "err", err)
	} else if perr := o.Client.Prepend(user, mbox, maxUID); perr != nil {
		serviceErr = perr
		slog.Warn("imap: fts prepend failed", "user", user, "err", perr)
	} else {
		timeout := o.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		deadline := time.Now().Add(timeout)
		// Give up early when the index makes NO progress: a broken FTS backend
		// keeps the checkpoint flat, so waiting the full timeout only makes the
		// client hang past its own TCP read deadline. A progressing index
		// advances the checkpoint and resets the stall counter, so slow-but-
		// working indexing still gets the full window. ~2s flat → fall back.
		const maxStallPolls = 8 // 8 × 250ms ≈ 2s of no movement
		best := last
		stalls := 0
		reason := "timed out"
		// A mailbox with nothing indexed has no baseline for that judgement:
		// zero progress there is what a job the indexer has not picked up yet
		// looks like, and what a broken backend looks like, and the two are
		// indistinguishable until something moves. Counting stalls from an
		// absent checkpoint refused the first touch of a cold account -- the
		// user's first action after a break -- while the second attempt, a
		// moment later, succeeded (#1379). The overall timeout still bounds
		// the wait; only the early exit is held back until there is movement
		// to reason about.
		cold := last == 0
		coldGrace := o.FirstIndexGrace
		if coldGrace <= 0 {
			coldGrace = 10 * time.Second
		}
		if coldGrace > timeout {
			coldGrace = timeout
		}
		coldDeadline := time.Now().Add(coldGrace)
		for time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			cur, _, serr := o.Client.Status(user, mbox)
			if serr != nil {
				serviceErr = serr
				reason = "status error"
				break
			}
			last = cur
			if cur >= maxUID {
				return false, nil
			}
			if cur > best {
				best = cur
				stalls = 0
				cold = false
				continue
			}
			if cold {
				// No baseline to judge the indexer by, so the grace is the
				// bound: long enough for the queue to pick the job up, short
				// enough that an engine which never starts does not hold the
				// client for the whole search timeout.
				if time.Now().After(coldDeadline) {
					reason = "first index did not start within the grace"
					break
				}
				continue
			}
			stalls++
			if stalls >= maxStallPolls {
				reason = "no progress"
				break
			}
		}
		if cold && reason == "timed out" {
			// Named apart from a stall: this mailbox never had an index, so
			// what timed out is the FIRST build, not a lagging one. An
			// operator reading "no progress" would look for a broken backend.
			reason = "first index not built within the timeout"
		}
		slog.Warn("imap: fts catch-up giving up, falling back to scan", "user", user, "folder", mbox.Name,
			"reason", reason, "indexed", last, "want", maxUID, "cold", cold)
	}
	if o.ReadFallback {
		return true, nil
	}
	// The same seam: the text already says "try again later", but only a code
	// makes that true for a client rather than advice it cannot act on. The
	// status/prepend errors above reach here from the same service, so an
	// outage inside it is answered as one.
	if errors.Is(serviceErr, ftsproto.ErrUnavailable) {
		return false, &imaplib.Error{Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeUnavailable,
			Text: "Full-text search is temporarily unavailable, try again"}
	}
	return false, &imaplib.Error{Type: imaplib.StatusResponseTypeNo,
		Text: "Mailbox is still being indexed, try again later"}
}

// buildFTSQuery converts the top-level Body/Text/Header criteria into the
// engine query and returns the criteria with those keys stripped, plus
// impossible=true if ANY criterion's every token expanded to nothing (pure
// stopwords): stopwords were never indexed, so that ANDed criterion can never
// match, and the whole query is unmatchable regardless of any other criteria
// that DID expand to real terms — a match-nothing, not a dropped, constraint.
func (s *session) buildFTSQuery(criteria *imaplib.SearchCriteria) (fts.Query, *imaplib.SearchCriteria, bool, bool) {
	chain := s.srv.opts.FTS.Chain
	var terms []fts.Term
	impossible := false
	add := func(exp expander, field fts.FieldKind, hdrName, value string) {
		words := exp.ExpandSearch(value)
		if len(words) == 0 && value != "" {
			impossible = true
			return
		}
		t := fts.Term{Field: field, HdrName: hdrName, Words: words}
		if strings.ContainsRune(strings.TrimSpace(value), ' ') {
			t.Phrase = value
		}
		terms = append(terms, t)
	}
	for _, v := range criteria.Body {
		add(chain, fts.FieldBody, "", v)
	}
	for _, v := range criteria.Text {
		add(chain, fts.FieldText, "", v)
	}
	for _, h := range criteria.Header {
		if h.Value == "" {
			terms = append(terms, fts.Term{Field: fts.FieldHeader,
				HdrName: strings.ToLower(h.Key)})
			continue
		}
		// Headers are not language text: always the no-stemming data chain,
		// never the configured language chain, regardless of the field name —
		// matching buildmail, which indexes every header through the same chain.
		add(headerDataChain, fts.FieldHeader, strings.ToLower(h.Key), h.Value)
	}

	stripped := *criteria
	stripped.Body = nil
	stripped.Text = nil
	stripped.Header = nil
	needsBody := !stripped.SentSince.IsZero() || !stripped.SentBefore.IsZero()
	return fts.Query{Terms: terms, AndTerms: true}, &stripped, needsBody, impossible
}

// ftsNotify fires the delivery/expunge hooks toward the yarilo-fts service —
// best-effort and asynchronous: the index heals via rescan if a hook is lost.
// Only the folder name travels; the service resolves the rest itself.
func (s *session) ftsNotify(f *mailbox.Folder, expunged bool, uid uint32) {
	o := s.srv.opts.FTS
	if o.Client == nil || s.userInfo == nil || f == nil || f.Name == "" {
		return
	}
	if !expunged && !o.Autoindex {
		return
	}
	user := s.userInfo.Username
	// The GUID is the folder's identity for the index, and it is what the
	// index path is keyed by -- an empty one would name a path built from a
	// value that is not there, so the hook is skipped instead (#1183). Said
	// once per folder: per message it would drown the reason it matters.
	mbox := ftsMailboxRef(f)
	if mbox.GUID == "" {
		warnFolderWithoutGUID(user, f.Name)
		return
	}
	go func() {
		var err error
		if expunged {
			err = o.Client.Expunge(user, mbox, uid)
		} else {
			err = o.Client.Index(user, mbox, uid, o.MaxRecent)
		}
		if err != nil {
			slog.Debug("imap: fts notify failed",
				"user", user, "folder", mbox.Name, "expunged", expunged, "err", err)
			return
		}
		// Breadcrumb: confirm the FTS index/expunge notify was sent, so an
		// indexing gap (message delivered but never handed to FTS) is visible.
		slog.Debug("imap: fts notify sent",
			"user", user, "folder", mbox.Name, "uid", uid, "expunged", expunged)
	}()
}

// relevancyScores normalizes raw per-UID engine weights to the RFC 4731/6203
// wire range, in order's enumeration order (one score per matched message,
// same order as the ESEARCH ALL data item). A per-result-set linear min-max
// scale to integers 1-100 — never 0, floored at 1 — with diff defaulting to
// 1.0 when every score is equal (avoids divide-by-zero; a uniform set maps to
// the floor value 1 for every message).
//
// order can include UIDs the engine returned no score for (matched only via a
// stripped, non-FTS criterion ANDed onto the search) — a plain map lookup
// would default those to 0.0 and corrupt the set's min-max range, dragging lo
// down to a fabricated zero and compressing everything else. Score-less UIDs
// are excluded from the lo/hi computation and floored to 1: "no ranking
// signal" is not "ranked lowest by the engine."
func relevancyScores(raw map[uint32]float64, order []uint32) []uint32 {
	if len(order) == 0 {
		return nil
	}
	var lo, hi float64
	haveRange := false
	for _, uid := range order {
		v, ok := raw[uid]
		if !ok {
			continue
		}
		if !haveRange || v < lo {
			lo = v
		}
		if !haveRange || v > hi {
			hi = v
		}
		haveRange = true
	}
	diff := hi - lo
	if diff == 0 {
		diff = 1.0
	}
	out := make([]uint32, len(order))
	for i, uid := range order {
		v, ok := raw[uid]
		if !ok || !haveRange {
			out[i] = 1
			continue
		}
		score := (v - lo) / diff * 100
		if score < 1 {
			out[i] = 1
		} else {
			out[i] = uint32(score)
		}
	}
	return out
}
