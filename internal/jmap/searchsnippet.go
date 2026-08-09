package jmap

import (
	"context"
	"encoding/json"
	"html"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// searchSnippet implements SearchSnippet/get (RFC 8621 §5.1).
//
// The engine reports no term positions (flatcurve: Positions false), so the
// highlighting is produced here: the message text is read again and matched
// against the same expanded terms the lookup used.
func (s *Server) searchSnippet(_ context.Context, h *userHandle, accountID string, args json.RawMessage) (any, *jmapcore.MethodError) {
	var req jmapcore.SearchSnippetGetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrInvalidArguments, Description: err.Error()}
	}
	if merr := checkAccount(req.AccountID, accountID); merr != nil {
		return nil, merr
	}
	// Every id costs a message read, so the same bound a Foo/get carries
	// applies: this is that role, not a new budget.
	if n := s.opts.Limits.MaxObjectsInGet; n > 0 && len(req.EmailIDs) > n {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrRequestTooLarge,
			Description: "more emailIds than maxObjectsInGet"}
	}
	if s.opts.FTS == nil || s.opts.FTS.Chain == nil {
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrUnsupportedFilter,
			Description: "full-text search is not configured"}
	}

	want := make(map[string]bool, len(req.EmailIDs))
	for _, id := range req.EmailIDs {
		want[id] = true
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		slog.Warn("jmap: SearchSnippet/get lookup failed", "account", accountID, "err", err)
		return nil, &jmapcore.MethodError{Type: jmapcore.ErrServerFail}
	}

	marker := newSnippetMarker(s, req.Filter)
	resp := jmapcore.SearchSnippetGetResponse{
		AccountID: accountID,
		List:      []jmapcore.SearchSnippet{},
		NotFound:  []string{},
	}
	eval := s.newFTSEvaluator(h)
	for _, id := range req.EmailIDs {
		ref, ok := found[id]
		if !ok {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		// The same read the Maybe confirmation uses: a second way of getting
		// the text would produce messages the search finds and the snippet
		// cannot show, for no reason in the data.
		hdr, body, err := eval.readMessage(scopeFolder{name: ref.folder}, ref.meta)
		if err != nil {
			slog.Warn("jmap: snippet cannot read the message", "id", id, "err", err)
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, jmapcore.SearchSnippet{
			EmailID: id,
			Subject: marker.mark(decodeWord(hdr.Get("subject")), 0),
			Preview: marker.mark(body, s.opts.SnippetMaxChars),
		})
	}
	return resp, nil
}

// snippetMarker highlights whole tokens whose stem matched, never substrings:
// the terms are stems, so a substring match would mark the tail of an
// unrelated word -- visible in a client at first glance.
type snippetMarker struct {
	// stems is what a token's own expansion is compared against.
	stems map[string]bool
	// chain expands the token being considered. The same one the stems came
	// from, so "running" marks under a query for "run" exactly when the index
	// thought it a match.
	chain *language.MultiChain
}

func newSnippetMarker(s *Server, f *jmapcore.EmailFilter) *snippetMarker {
	m := &snippetMarker{stems: map[string]bool{}}
	if f == nil || s.opts.FTS == nil {
		return m
	}
	m.chain = s.opts.FTS.Chain
	for _, value := range f.TextValues() {
		for _, w := range s.opts.FTS.Chain.ExpandSearch(value) {
			for _, v := range w.Variants {
				m.stems[strings.ToLower(v)] = true
			}
		}
	}
	return m
}

// mark renders text with <mark> around the matching tokens, or nil when
// nothing matched: RFC 8621 §5.1 allows a null field, and a fragment with no
// highlight would be an invented one.
//
// Escaping happens before the markup is inserted -- the other order escapes
// our own tags and the client shows a literal &lt;mark&gt;.
func (m *snippetMarker) mark(text string, maxChars int) *string {
	if text == "" || len(m.stems) == 0 {
		return nil
	}
	var out strings.Builder
	matched := false
	for _, tok := range splitTokens(text) {
		if tok.word && m.matches(tok.text) {
			matched = true
			out.WriteString("<mark>")
			out.WriteString(html.EscapeString(tok.text))
			out.WriteString("</mark>")
			continue
		}
		out.WriteString(html.EscapeString(tok.text))
	}
	if !matched {
		return nil
	}
	s := out.String()
	if maxChars > 0 {
		s = truncateMarked(s, maxChars)
	}
	return &s
}

// matches reports whether this token's own expansion meets the query's. Both
// sides go through the same chain, so "running" matches a query for "run"
// exactly when the index thought so.
func (m *snippetMarker) matches(token string) bool {
	if m.stems[strings.ToLower(token)] {
		return true
	}
	if m.chain == nil {
		return false
	}
	for _, w := range m.chain.ExpandSearch(token) {
		for _, v := range w.Variants {
			if m.stems[strings.ToLower(v)] {
				return true
			}
		}
	}
	return false
}

// token is a run of text, flagged as a word or as the separators between them,
// so marking can replace whole words and keep everything else verbatim.
type token struct {
	text string
	word bool
}

func splitTokens(text string) []token {
	var out []token
	var cur strings.Builder
	curWord := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, token{text: cur.String(), word: curWord})
			cur.Reset()
		}
	}
	for _, r := range text {
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r)
		if isWord != curWord {
			flush()
			curWord = isWord
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// truncateMarked cuts to maxChars of visible text, counting neither the markup
// nor the escapes -- a limit that counted them would shrink with every
// ampersand in the message.
func truncateMarked(s string, maxChars int) string {
	var out strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "<mark>"), strings.HasPrefix(s[i:], "</mark>"):
			end := strings.IndexByte(s[i:], '>') + i + 1
			out.WriteString(s[i:end])
			i = end
		case s[i] == '&':
			end := strings.IndexByte(s[i:], ';')
			if end < 0 {
				end = i + 1
			} else {
				end += i + 1
			}
			out.WriteString(s[i:end])
			i = end
			visible++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			out.WriteString(s[i : i+size])
			i += size
			visible++
		}
		if visible >= maxChars {
			break
		}
	}
	res := out.String()
	// A cut inside a highlight would ship an unclosed tag.
	if strings.Count(res, "<mark>") > strings.Count(res, "</mark>") {
		res += "</mark>"
	}
	return res
}
