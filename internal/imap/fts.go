package imap

import (
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/ftsproto"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// FTSOptions wires full-text search into IMAP sessions. Nil Client disables
// FTS entirely — SEARCH keeps the sequential scan.
type FTSOptions struct {
	Client ftsproto.Client
	// Chain must match the yarilo-fts service's language settings so query
	// expansion tokenizes exactly like indexing did.
	Chain *language.Chain
	// AddMissing / ReadFallback / Timeout / Strict — see docs/FTS.md §11.
	AddMissing   string
	ReadFallback bool
	Timeout      time.Duration
	Strict       bool
	// Autoindex triggers INDEX on delivered events; MaxRecent is the
	// autoindex throttle forwarded to the service.
	Autoindex bool
	MaxRecent int
}

func (o FTSOptions) enabled() bool { return o.Client != nil && o.Chain != nil }

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

	query, stripped, strippedNeedsBody := s.buildFTSQuery(criteria)
	if len(query.Terms) == 0 {
		// Everything expanded to stopwords: no FTS constraint remains, and
		// per the indexing model those words were never indexed — the
		// stripped criteria alone decide.
		return &ftsFilter{
			covered:           allUIDs(msgs),
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
	// Visibility diagnostic (#625): how many candidates the FTS index returned for
	// this search, so a "search finds nothing" case shows whether FTS had no hits
	// (0 candidates → likely not indexed) vs. hits that later failed re-verify.
	// Counts only — never the query terms (private mail content).
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
	if err != nil {
		slog.Warn("imap: fts status failed", "user", user, "err", err)
	} else if err := o.Client.Prepend(user, mbox, maxUID); err != nil {
		slog.Warn("imap: fts prepend failed", "user", user, "err", err)
	} else {
		timeout := o.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			if last, _, err = o.Client.Status(user, mbox); err == nil && last >= maxUID {
				return false, nil
			}
		}
		slog.Warn("imap: fts catch-up timed out", "user", user, "folder", mbox.Name,
			"indexed", last, "want", maxUID)
	}
	if o.ReadFallback {
		return true, nil
	}
	return false, &imaplib.Error{Type: imaplib.StatusResponseTypeNo,
		Text: "Mailbox is still being indexed, try again later"}
}

// buildFTSQuery converts the top-level Body/Text/Header criteria into the
// engine query and returns the criteria with those keys stripped. A word
// that expands to nothing (pure stopwords) was never indexed and imposes no
// constraint — its criterion is dropped from both sides.
func (s *session) buildFTSQuery(criteria *imaplib.SearchCriteria) (fts.Query, *imaplib.SearchCriteria, bool) {
	chain := s.srv.opts.FTS.Chain
	var terms []fts.Term
	add := func(field fts.FieldKind, hdrName, value string) {
		words := chain.ExpandSearch(value)
		if len(words) == 0 && value != "" {
			return
		}
		t := fts.Term{Field: field, HdrName: hdrName, Words: words}
		if strings.ContainsRune(strings.TrimSpace(value), ' ') {
			t.Phrase = value
		}
		terms = append(terms, t)
	}
	for _, v := range criteria.Body {
		add(fts.FieldBody, "", v)
	}
	for _, v := range criteria.Text {
		add(fts.FieldText, "", v)
	}
	for _, h := range criteria.Header {
		if h.Value == "" {
			terms = append(terms, fts.Term{Field: fts.FieldHeader,
				HdrName: strings.ToLower(h.Key)})
			continue
		}
		add(fts.FieldHeader, strings.ToLower(h.Key), h.Value)
	}

	stripped := *criteria
	stripped.Body = nil
	stripped.Text = nil
	stripped.Header = nil
	needsBody := !stripped.SentSince.IsZero() || !stripped.SentBefore.IsZero()
	return fts.Query{Terms: terms, AndTerms: true}, &stripped, needsBody
}

func allUIDs(msgs []*mailbox.MessageMeta) map[uint32]bool {
	out := make(map[uint32]bool, len(msgs))
	for _, m := range msgs {
		out[m.UID] = true
	}
	return out
}

// ftsNotify fires the delivery/expunge hooks toward the yarilo-fts service —
// best-effort and asynchronous: the index heals via rescan if a hook is lost.
// Only the folder name travels; the service resolves the rest itself.
func (s *session) ftsNotify(folderName string, expunged bool, uid uint32) {
	o := s.srv.opts.FTS
	if o.Client == nil || s.userInfo == nil || folderName == "" {
		return
	}
	if !expunged && !o.Autoindex {
		return
	}
	user := s.userInfo.Username
	mbox := fts.MailboxRef{Name: folderName}
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
		// Breadcrumb (#625): confirm the FTS index/expunge notify was sent, so an
		// indexing gap (message delivered but never handed to FTS) is visible.
		slog.Debug("imap: fts notify sent",
			"user", user, "folder", mbox.Name, "uid", uid, "expunged", expunged)
	}()
}
