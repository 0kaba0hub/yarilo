package buildmail

import (
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/fts"
)

type recordedKey struct {
	key    fts.BuildKey
	tokens []string
}

type fakeUpdate struct {
	keys   []*recordedKey
	reject map[string]bool // HdrName / ContentType values to reject
}

func (f *fakeUpdate) SetBuildKey(k fts.BuildKey) (bool, error) {
	if f.reject[k.HdrName] || f.reject[k.ContentType] {
		return false, nil
	}
	f.keys = append(f.keys, &recordedKey{key: k})
	return true, nil
}

func (f *fakeUpdate) BuildMore(data []byte) error {
	last := f.keys[len(f.keys)-1]
	last.tokens = append(last.tokens, string(data))
	return nil
}

func (f *fakeUpdate) Commit() error   { return nil }
func (f *fakeUpdate) Rollback() error { return nil }

func (f *fakeUpdate) find(t fts.BuildKeyType, hdr string) *recordedKey {
	for _, k := range f.keys {
		if k.key.Type == t && k.key.HdrName == hdr {
			return k
		}
	}
	return nil
}

func (f *fakeUpdate) bodyTokens() []string {
	var out []string
	for _, k := range f.keys {
		if k.key.Type == fts.KeyBodyPart {
			out = append(out, k.tokens...)
		}
	}
	return out
}

func mustChain(t *testing.T) *language.MultiChain {
	t.Helper()
	set := language.DefaultSettings()
	c, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hasToken(tokens []string, want string) bool {
	for _, tok := range tokens {
		if tok == want {
			return true
		}
	}
	return false
}

const multipartMsg = "From: Alice Smith <alice@example.com>\r\n" +
	"To: bob@example.org\r\n" +
	"Subject: Quarterly Planning\r\n" +
	"X-Tracking-Id: zz9999\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"The projects are running smoothly.\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><head><style>b{color:red}</style></head>" +
	"<body>Hidden <b>signals</b><script>alert('nope')</script></body></html>\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQKJcTl8uXrp/Og0MTGCg==\r\n" +
	"--BOUND--\r\n"

func TestBuildMultipart(t *testing.T) {
	upd := &fakeUpdate{}
	b := New(Options{}, mustChain(t))
	if _, err := b.Build(7, strings.NewReader(multipartMsg), upd); err != nil {
		t.Fatal(err)
	}

	subj := upd.find(fts.KeyHeader, "subject")
	if subj == nil || subj.key.UID != 7 {
		t.Fatalf("missing subject header key: %+v", upd.keys)
	}
	// Headers go through dataChain (#696 point 2): lowercase only, never
	// stemmed — "Quarterly Planning" indexes as-is, not the snowball stems
	// "quarter"/"plan" the body's own text gets.
	if !hasToken(subj.tokens, "quarterly") || !hasToken(subj.tokens, "planning") {
		t.Fatalf("subject tokens = %q, want unstemmed quarterly/planning", subj.tokens)
	}
	from := upd.find(fts.KeyHeader, "from")
	if from == nil || !hasToken(from.tokens, "alice@example.com") {
		t.Fatalf("from tokens = %v, want whole address token", from)
	}

	body := upd.bodyTokens()
	if !hasToken(body, "project") || !hasToken(body, "smooth") {
		t.Fatalf("plain body tokens = %q, want stemmed project/smooth", body)
	}
	if !hasToken(body, "hidden") || !hasToken(body, "signal") {
		t.Fatalf("html body tokens = %q, want hidden/signal", body)
	}
	for _, bad := range []string{"alert", "nope", "color", "red"} {
		if hasToken(body, bad) {
			t.Fatalf("script/style content leaked into index: %q in %q", bad, body)
		}
	}
	for _, k := range upd.keys {
		if k.key.Type == fts.KeyBodyPart && k.key.ContentType == "application/pdf" {
			t.Fatal("binary part must be skipped")
		}
	}
}

// TestAddressHeaderStructuredParsing (#725 item 7) proves address headers
// are parsed via net/mail.ParseAddressList on the RAW bytes before RFC2047
// decoding, not by tokenizing the already-decoded text as one blob.
func TestAddressHeaderStructuredParsing(t *testing.T) {
	tests := []struct {
		name       string
		msg        string
		wantTokens []string // every one of these must be present
	}{
		{
			// The encoded-word decodes to "Support (Team)" — parentheses
			// that would be misread as RFC 5322 comment delimiters if
			// decoding happened BEFORE address-list parsing, corrupting or
			// dropping the address entirely. Parsing the raw (still-
			// encoded) bytes first avoids that; net/mail.ParseAddressList
			// decodes the RFC2047 phrase as part of parsing.
			name: "RFC2047 display name with parens survives parsing",
			msg: "From: =?utf-8?Q?Support_=28Team=29?= <user@example.com>\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n",
			wantTokens: []string{"support", "team", "user@example.com"},
		},
		{
			// A header that doesn't parse as a valid address list falls
			// back to tokenizing the decoded text as-is — imperfect
			// tokenization beats dropping the header.
			name: "unparseable address header falls back to decoded text",
			msg: "From: @@@ garbage @@@\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n",
			wantTokens: []string{"garbage"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upd := &fakeUpdate{}
			b := New(Options{}, mustChain(t))
			if _, err := b.Build(1, strings.NewReader(tc.msg), upd); err != nil {
				t.Fatal(err)
			}
			from := upd.find(fts.KeyHeader, "from")
			if from == nil {
				t.Fatal("missing from header key")
			}
			for _, want := range tc.wantTokens {
				if !hasToken(from.tokens, want) {
					t.Fatalf("from tokens = %q, want %q among them", from.tokens, want)
				}
			}
		})
	}
}

func TestHeaderIncludeExclude(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		hdr      string
		expected bool
	}{
		{"default indexes all", Options{}, "x-tracking-id", true},
		{"exclude drops", Options{HeaderExcludes: []string{"X-Tracking-*"}}, "x-tracking-id", false},
		{"include overrides exclude", Options{
			HeaderIncludes: []string{"X-Tracking-Id"},
			HeaderExcludes: []string{"X-*"},
		}, "x-tracking-id", true},
		{"exact exclude", Options{HeaderExcludes: []string{"Subject"}}, "subject", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upd := &fakeUpdate{}
			b := New(tc.opts, mustChain(t))
			if _, err := b.Build(1, strings.NewReader(multipartMsg), upd); err != nil {
				t.Fatal(err)
			}
			got := upd.find(fts.KeyHeader, tc.hdr) != nil
			if got != tc.expected {
				t.Fatalf("header %q indexed = %v, want %v", tc.hdr, got, tc.expected)
			}
		})
	}
}

func TestRejectedKeySkipsData(t *testing.T) {
	upd := &fakeUpdate{reject: map[string]bool{"subject": true}}
	b := New(Options{}, mustChain(t))
	if _, err := b.Build(1, strings.NewReader(multipartMsg), upd); err != nil {
		t.Fatal(err)
	}
	if upd.find(fts.KeyHeader, "subject") != nil {
		t.Fatal("rejected key must not be recorded or streamed")
	}
}

func TestBodySizeCap(t *testing.T) {
	msg := "Subject: cap\r\n\r\n" + strings.Repeat("alpha beta ", 100) + "omega"
	upd := &fakeUpdate{}
	b := New(Options{MaxSize: 20}, mustChain(t))
	if _, err := b.Build(1, strings.NewReader(msg), upd); err != nil {
		t.Fatal(err)
	}
	body := upd.bodyTokens()
	if hasToken(body, "omega") {
		t.Fatalf("size cap not applied: %q", body)
	}
	if !hasToken(body, "alpha") {
		t.Fatalf("capped body should keep leading tokens: %q", body)
	}
}

// boundedReader fails the test if more than max bytes are ever read from
// it — used to prove Build() does not buffer the whole message up front for
// a single-language config (review finding: an unconditional io.ReadAll
// would silently defeat MaxSize's memory-bounding purpose for a large
// message).
type boundedReader struct {
	t      *testing.T
	r      io.Reader
	max    int64
	total  int64
	failed bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.total += int64(n)
	if b.total > b.max && !b.failed {
		b.failed = true
		b.t.Errorf("read %d bytes, want at most %d — message body was buffered beyond the size cap", b.total, b.max)
	}
	return n, err
}

func TestBuildDoesNotBufferWholeMessageForSingleLanguage(t *testing.T) {
	const sizeCap = 20
	hugeBody := strings.Repeat("alpha beta ", 100_000) // ~1.1MB
	msg := "Subject: cap\r\n\r\n" + hugeBody
	upd := &fakeUpdate{}
	// mustChain builds a single-language MultiChain (en only): Build must
	// take the streaming path, never reading much past the size cap.
	b := New(Options{MaxSize: sizeCap}, mustChain(t))
	// Slack accounts for header bytes plus a few internal buffered-reader
	// chunks (go-message's parser + copyChunks' own 8KiB buffer) — nowhere
	// near the full ~1.1MB body, which is exactly what this test guards.
	br := &boundedReader{t: t, r: strings.NewReader(msg), max: 64 * 1024}
	if _, err := b.Build(1, br, upd); err != nil {
		t.Fatal(err)
	}
}

func TestNestedMessage(t *testing.T) {
	msg := "Subject: outer\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		"Subject: inner secret\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"nested payload here\r\n"
	upd := &fakeUpdate{}
	b := New(Options{}, mustChain(t))
	if _, err := b.Build(1, strings.NewReader(msg), upd); err != nil {
		t.Fatal(err)
	}
	inner := upd.find(fts.KeyMIMEHeader, "subject")
	if inner == nil || !hasToken(inner.tokens, "secret") {
		t.Fatalf("nested message headers must index as MIME headers: %+v", upd.keys)
	}
	if !hasToken(upd.bodyTokens(), "payload") {
		t.Fatalf("nested body not indexed: %q", upd.bodyTokens())
	}
}

func TestEncodedWordHeader(t *testing.T) {
	msg := "Subject: =?UTF-8?B?0J/RgNC40LLRltGCINGB0LLRltGC?=\r\n\r\nbody\r\n"
	upd := &fakeUpdate{}
	b := New(Options{}, mustChain(t))
	if _, err := b.Build(1, strings.NewReader(msg), upd); err != nil {
		t.Fatal(err)
	}
	subj := upd.find(fts.KeyHeader, "subject")
	if subj == nil || !hasToken(subj.tokens, "привіт") {
		t.Fatalf("encoded-word subject not decoded: %+v", subj)
	}
}
