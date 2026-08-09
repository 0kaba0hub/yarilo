package jmap

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func snippetGet(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["SearchSnippet/get",`+args+`,"c0"]]}`)
}

func firstSnippet(t *testing.T, got map[string]any) map[string]any {
	t.Helper()
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list has %d snippets, want 1: %v", len(list), got)
	}
	return list[0].(map[string]any)
}

// snippetServer is a server with one message and a stub that reports the index
// current, so the snippet path is what the test is about.
func snippetServer(t *testing.T, raw string) *Server {
	t.Helper()
	stub := &stubFTS{statusUID: 1, byFolder: map[string]fts.Result{"INBOX": {Definite: []uint32{1}}}}
	s := rawMessageServer(t, stub, raw)
	s.opts.SnippetMaxChars = 256
	return s
}

func snippetIDs(t *testing.T, s *Server) string {
	t.Helper()
	ids := idsOf(t, emailQuery(t, s, `{"accountId":"u1@example.com","filter":{"text":"needle"}}`))
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want one message to snippet", ids)
	}
	return ids[0]
}

// The text is escaped BEFORE the markup goes in. The other order escapes our
// own tags, and the client renders a literal &lt;mark&gt; instead of a
// highlight -- which a fixture without markup-shaped content cannot show.
func TestSearchSnippetEscapesBeforeMarking(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:      "a script tag in the body arrives escaped",
			body:      "the needle and <script>alert(1)</script>",
			wantIn:    []string{"<mark>needle</mark>", "&lt;script&gt;"},
			wantNotIn: []string{"<script>"},
		},
		{
			// Without this row the two orderings look identical.
			name:      "a literal mark tag in the body does not merge with ours",
			body:      "the needle and <mark>not ours</mark>",
			wantIn:    []string{"<mark>needle</mark>", "&lt;mark&gt;"},
			wantNotIn: []string{"&lt;mark&gt;needle"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snippetServer(t, "Subject: hi\r\nFrom: alice@example.com\r\n\r\n"+tc.body+"\r\n")
			id := snippetIDs(t, s)
			got := snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
				"filter":{"text":"needle"}}`)
			preview, _ := firstSnippet(t, got)["preview"].(string)
			for _, want := range tc.wantIn {
				if !strings.Contains(preview, want) {
					t.Errorf("preview %q does not contain %q", preview, want)
				}
			}
			for _, bad := range tc.wantNotIn {
				if strings.Contains(preview, bad) {
					t.Errorf("preview %q contains %q", preview, bad)
				}
			}
		})
	}
}

// A null field is allowed (RFC 8621 5.1) and is the honest answer for a hit
// that is not in that field. Per field, not per message: a subject hit still
// returns a highlighted subject.
func TestSearchSnippetNullsThePartWithNoMatch(t *testing.T) {
	s := snippetServer(t, "Subject: the needle in the subject\r\nFrom: alice@example.com\r\n\r\nnothing to see\r\n")
	id := snippetIDs(t, s)
	got := firstSnippet(t, snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`))

	subject, _ := got["subject"].(string)
	if !strings.Contains(subject, "<mark>needle</mark>") {
		t.Errorf("subject = %q, want the hit highlighted", subject)
	}
	if got["preview"] != nil {
		t.Errorf("preview = %v, want null rather than an invented fragment", got["preview"])
	}
}

// Whole tokens are marked, never substrings: the terms are stems, so marking
// what merely starts with one cuts unrelated words in half.
func TestSearchSnippetMarksWholeTokens(t *testing.T) {
	s := snippetServer(t, "Subject: hi\r\nFrom: alice@example.com\r\n\r\nneedles and needlework and needle\r\n")
	id := snippetIDs(t, s)
	preview, _ := firstSnippet(t, snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`))["preview"].(string)

	// Whatever the stemmer decides about needles/needlework, no mark may end
	// inside a word: that is the visible defect.
	for _, bad := range []string{"<mark>needle</mark>s", "<mark>needle</mark>work"} {
		if strings.Contains(preview, bad) {
			t.Errorf("preview %q marks part of a word", preview)
		}
	}
	if !strings.Contains(preview, "<mark>needle</mark>") {
		t.Errorf("preview %q does not mark the exact word", preview)
	}
}

// The id count is bounded by maxObjectsInGet, since every id costs a message
// read. Both sides of the boundary, so a limit that refused everything would
// not pass.
func TestSearchSnippetBoundsTheIDCount(t *testing.T) {
	s := snippetServer(t, "Subject: hi\r\nFrom: alice@example.com\r\n\r\nthe needle\r\n")
	id := snippetIDs(t, s)

	s.opts.Limits.MaxObjectsInGet = 1
	if got := snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`); got["list"] == nil {
		t.Errorf("a request at the limit was refused: %v", got)
	}

	err := snippetGetError(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`","other"],
		"filter":{"text":"needle"}}`)
	if err["type"] != "requestTooLarge" {
		t.Errorf("type = %v, want requestTooLarge", err["type"])
	}
}

// A long message: the fragment must be the window around the hit, not the head
// of the mail. Cutting from the start returns 256 characters with no highlight
// in them, stated as a search result -- worse than null, because the field
// claims a match.
func TestSearchSnippetWindowsAroundTheHit(t *testing.T) {
	filler := strings.Repeat("padding words here ", 120) // ~2 KB before the hit
	s := snippetServer(t, "Subject: hi\r\nFrom: alice@example.com\r\n\r\n"+filler+"the needle at last\r\n")
	id := snippetIDs(t, s)
	preview, _ := firstSnippet(t, snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`))["preview"].(string)

	if !strings.Contains(preview, "<mark>needle</mark>") {
		t.Fatalf("preview %q carries no highlight: it is the head of the message, not a snippet", preview)
	}
	if visibleLen(preview) > s.opts.SnippetMaxChars+2 { // the two ellipses
		t.Errorf("preview is %d visible chars, want at most %d", visibleLen(preview), s.opts.SnippetMaxChars)
	}
	if !strings.HasPrefix(preview, "\u2026") {
		t.Errorf("preview %q does not say it starts mid-message", preview)
	}
}

// The confirmation searches every text part, markup included; a reader must
// not be shown that markup. text/plain wins, and the HTML twin stays out.
func TestSearchSnippetPreviewSkipsTheHTMLSource(t *testing.T) {
	raw := "Subject: hi\r\nFrom: alice@example.com\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nthe needle in plain text\r\n" +
		"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<div style=\"color:red\"><p>the needle in html</p></div>\r\n--b1--\r\n"
	s := snippetServer(t, raw)
	id := snippetIDs(t, s)
	preview, _ := firstSnippet(t, snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`))["preview"].(string)

	if !strings.Contains(preview, "<mark>needle</mark>") {
		t.Errorf("preview %q has no highlight", preview)
	}
	for _, bad := range []string{"&lt;div", "&lt;p&gt;", "color:red"} {
		if strings.Contains(preview, bad) {
			t.Errorf("preview %q shows HTML source", preview)
		}
	}
}

// An HTML-only message still gets a fragment, with the tags stripped rather
// than escaped into view.
func TestSearchSnippetStripsAnHTMLOnlyBody(t *testing.T) {
	raw := "Subject: hi\r\nFrom: alice@example.com\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<div><p>the needle in html</p></div>\r\n"
	s := snippetServer(t, raw)
	id := snippetIDs(t, s)
	preview, _ := firstSnippet(t, snippetGet(t, s, `{"accountId":"u1@example.com","emailIds":["`+id+`"],
		"filter":{"text":"needle"}}`))["preview"].(string)

	if !strings.Contains(preview, "<mark>needle</mark>") {
		t.Errorf("preview %q has no highlight", preview)
	}
	if strings.Contains(preview, "&lt;") {
		t.Errorf("preview %q shows escaped tags instead of text", preview)
	}
}
