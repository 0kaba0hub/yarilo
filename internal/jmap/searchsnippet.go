package jmap

import (
	"context"
	"encoding/json"
	"html"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yarilomail/yarilo/internal/fts/buildmail"
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
		hdr, parts, err := eval.readParts(scopeFolder{name: ref.folder}, ref.meta)
		if err != nil {
			slog.Warn("jmap: snippet cannot read the message", "id", id, "err", err)
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		resp.List = append(resp.List, jmapcore.SearchSnippet{
			EmailID: id,
			Subject: marker.mark(decodeWord(hdr.Get("subject")), 0),
			Preview: marker.mark(displayText(parts), s.opts.SnippetMaxChars),
		})
	}
	return resp, nil
}

// displayText is what a reader is shown, which is not what a condition is
// confirmed against: the confirmation searches every text part, markup
// included, while a fragment must not put HTML source in front of a person.
// text/plain wins; an HTML-only message is stripped, the same shape Email
// preview takes.
func displayText(parts []walkedPart) string {
	var htmlBody string
	for _, p := range parts {
		if p.mediaType == "text/plain" && p.body != "" {
			return p.body
		}
		if p.mediaType == "text/html" && htmlBody == "" {
			htmlBody = p.body
		}
	}
	if htmlBody == "" {
		return ""
	}
	var out strings.Builder
	if err := buildmail.HTMLToText(strings.NewReader(htmlBody), func(b []byte) error {
		out.Write(b)
		out.WriteByte(' ')
		return nil
	}); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out.String()), " ")
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
	// seen memoises the verdict per token: a get over hundreds of ids would
	// otherwise stem tens of thousands of words, and a long message repeats
	// the same ones throughout.
	seen map[string]bool
}

func newSnippetMarker(s *Server, f *jmapcore.EmailFilter) *snippetMarker {
	m := &snippetMarker{stems: map[string]bool{}, seen: map[string]bool{}}
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
	for _, tok := range splitTokens(text) {
		if tok.word && m.matches(tok.text) {
			out.WriteString("<mark>")
			out.WriteString(html.EscapeString(tok.text))
			out.WriteString("</mark>")
			continue
		}
		out.WriteString(html.EscapeString(tok.text))
	}
	s := out.String()
	if maxChars > 0 {
		s = windowAround(s, maxChars)
	}
	// The one decision, taken on what is being returned rather than on what was
	// found somewhere in the message: a fragment with no highlight in it is the
	// invented answer null exists to avoid.
	if !strings.Contains(s, "<mark>") {
		return nil
	}
	return &s
}

// windowAround cuts maxChars of visible text around the first highlight --
// "the relevant section" the method is named for (RFC 8621 5.1). Cutting from
// the start instead would return the head of a long message with no highlight
// in it, stated as a search fragment.
func windowAround(s string, maxChars int) string {
	hit := strings.Index(s, "<mark>")
	if hit < 0 {
		out, _ := truncateMarked(s, maxChars)
		return out
	}
	// A quarter of the window as lead-in, so the hit is not flush left, backed
	// up to a word boundary: a fragment starting mid-word reads as damaged.
	lead := maxChars / 4
	start := backUpVisible(s, hit, lead)
	out, spent := truncateMarked(s[start:], maxChars)
	if start > 0 {
		out = ellipsis + out
	}
	// Bytes against bytes: the markup inside the window costs bytes and no
	// visible characters, so measuring the window in one and the text in the
	// other claimed there was more to come in every answer, the last one
	// included.
	if start+spent < len(s) {
		out += ellipsis
	}
	return out
}

// ellipsis marks a fragment that does not start or end at the message's own
// edges, so a reader is not told the mail begins mid-sentence.
const ellipsis = "\u2026"

// backUpVisible walks back from hit over at most n visible characters, then to
// the nearest word boundary, never splitting an entity or a tag.
func backUpVisible(s string, hit, n int) int {
	i := hit
	for visible := 0; i > 0 && visible < n; visible++ {
		j := i - 1
		for j > 0 && !utf8.RuneStart(s[j]) {
			j--
		}
		// Stepping back into markup would leave a fragment of it.
		if s[j] == '>' {
			break
		}
		// A semicolon may end an entity or just be prose; only the former is a
		// reason to stop, and it is recognised from its '&'.
		if s[j] == ';' {
			if amp := strings.LastIndexByte(s[:j], '&'); amp >= 0 && j-amp <= 10 {
				break
			}
		}
		i = j
	}
	// Forward to just past the next boundary, so the window starts at a word.
	// Decoded, not byte-wise: where the separator is not ASCII -- an em dash
	// between words -- a byte test walks past it and opens the window inside
	// the preceding word.
	for i < hit {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if isBoundary(r) {
			break
		}
	}
	if i >= hit {
		return hit
	}
	return i
}

func isBoundary(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) }

// visibleLen counts what a reader sees: markup and entities are one thing to
// them, not the bytes they are written with.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "<mark>"), strings.HasPrefix(s[i:], "</mark>"):
			i += strings.IndexByte(s[i:], '>') + 1
		case s[i] == '&':
			if end := strings.IndexByte(s[i:], ';'); end >= 0 {
				i += end + 1
			} else {
				i++
			}
			n++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
			n++
		}
	}
	return n
}

// matches reports whether this token's own expansion meets the query's. Both
// sides go through the same chain, so "running" matches a query for "run"
// exactly when the index thought so.
func (m *snippetMarker) matches(token string) bool {
	key := strings.ToLower(token)
	if hit, ok := m.seen[key]; ok {
		return hit
	}
	hit := m.expandAndCompare(key)
	m.seen[key] = hit
	return hit
}

func (m *snippetMarker) expandAndCompare(token string) bool {
	if m.stems[token] {
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
func truncateMarked(s string, maxChars int) (string, int) {
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
	spent := len(res)
	// A cut inside a highlight would ship an unclosed tag. The closing tag is
	// ours, not the message's, so it does not count as text consumed.
	if strings.Count(res, "<mark>") > strings.Count(res, "</mark>") {
		res += "</mark>"
	}
	return res, spent
}
