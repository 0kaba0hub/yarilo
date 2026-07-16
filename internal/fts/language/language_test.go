package language

import (
	"reflect"
	"strings"
	"testing"
)

func tokenizeGeneric(t *testing.T, maxLen int, chunks ...string) []string {
	t.Helper()
	g := NewGeneric(maxLen)
	var out []string
	emit := func(tok string) error {
		out = append(out, tok)
		return nil
	}
	for _, c := range chunks {
		if err := g.Feed([]byte(c), emit); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Flush(emit); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGenericTokenizer(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"simple words", "hello world", []string{"hello", "world"}},
		{"punctuation breaks", "foo,bar;baz.quux!", []string{"foo", "bar", "baz", "quux"}},
		{"underscore is word char", "foo_bar", []string{"foo_bar"}},
		{"hyphen breaks", "foo-bar", []string{"foo", "bar"}},
		{"digits", "abc123 456", []string{"abc123", "456"}},
		{"internal apostrophe kept", "o'brien", []string{"o'brien"}},
		{"trailing apostrophe trimmed", "dogs' bark", []string{"dogs", "bark"}},
		{"leading apostrophe breaks", "'quoted'", []string{"quoted"}},
		{"double apostrophe splits", "a''b", []string{"a", "b"}},
		{"unicode right quote as apostrophe", "l’eau", []string{"l'eau"}},
		{"unicode words", "привіт світ", []string{"привіт", "світ"}},
		{"unicode dash breaks", "foo—bar", []string{"foo", "bar"}},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenizeGeneric(t, 0, tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tokens = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGenericTruncation(t *testing.T) {
	long := strings.Repeat("a", 40)
	got := tokenizeGeneric(t, 0, long)
	if len(got) != 1 || len(got[0]) != DefaultTokenMaxLen {
		t.Fatalf("got %q (len %d), want single %d-byte token", got, len(got[0]), DefaultTokenMaxLen)
	}
	// Truncation must not split a multibyte rune.
	longUni := strings.Repeat("ї", 20) // 2 bytes each → 40 bytes untruncated
	got = tokenizeGeneric(t, 0, longUni)
	if len(got) != 1 || !strings.HasSuffix(got[0], "ї") || len(got[0]) != 30 {
		t.Fatalf("unicode truncation got %q (len %d)", got, len(got[0]))
	}
	// A truncated token keeps a trailing apostrophe (reference behaviour).
	trunc := strings.Repeat("b", 29) + "'x"
	got = tokenizeGeneric(t, 0, trunc)
	if len(got) != 1 || got[0] != strings.Repeat("b", 29)+"'" {
		t.Fatalf("truncated apostrophe got %q", got)
	}
}

func TestGenericChunkBoundaries(t *testing.T) {
	// Token and multibyte rune split across Feed calls must reassemble.
	got := tokenizeGeneric(t, 0, "hel", "lo wo", "rld")
	if !reflect.DeepEqual(got, []string{"hello", "world"}) {
		t.Fatalf("split token got %q", got)
	}
	word := "світ"
	b := []byte(word)
	got = tokenizeGeneric(t, 0, string(b[:3]), string(b[3:]))
	if !reflect.DeepEqual(got, []string{word}) {
		t.Fatalf("split rune got %q", got)
	}
}

func TestGenericBase64Skip(t *testing.T) {
	run := strings.Repeat("QWJjZGVmZ2hpamtsbW5vcHFyc3R1", 2) // 56 base64 chars
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"long run after colon skipped", "sig: " + run + " tail", []string{"sig", "tail"}},
		{"short run kept as token", "x: QWJjZA== y", []string{"x", "QWJjZA", "y"}},
		{"run with bad trailer kept", "x: " + run + "(y", []string{"x", strings.Repeat("QWJjZGVmZ2hpamtsbW5vcHFyc3R1", 2)[:DefaultTokenMaxLen], "y"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenizeGeneric(t, 0, tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tokens = %q, want %q", got, tc.want)
			}
		})
	}
}

func tokenizeAddress(t *testing.T, search bool, in string) []string {
	t.Helper()
	a := NewAddress(NewGeneric(0), 0, search)
	var out []string
	emit := func(tok string) error {
		out = append(out, tok)
		return nil
	}
	if err := a.Feed([]byte(in), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(emit); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAddressTokenizerIndex(t *testing.T) {
	// Index mode: the whole address is one token AND its parts flow through
	// the generic tokenizer.
	got := tokenizeAddress(t, false, "mail from John.Doe@Example.COM today")
	want := []string{"John.Doe@Example.COM", "mail", "from", "John", "Doe", "Example", "COM", "today"}
	if !sameElements(got, want) {
		t.Fatalf("index tokens = %q, want elements %q", got, want)
	}
}

func TestAddressTokenizerSearch(t *testing.T) {
	// Search mode: a complete address is withheld from the parent so only
	// the whole-address token matches.
	got := tokenizeAddress(t, true, "John.Doe@Example.COM")
	want := []string{"John.Doe@Example.COM"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search tokens = %q, want %q", got, want)
	}
	// Non-address text still tokenizes normally.
	got = tokenizeAddress(t, true, "plain words")
	if !sameElements(got, []string{"plain", "words"}) {
		t.Fatalf("search non-address = %q", got)
	}
}

func sameElements(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func TestChainFilters(t *testing.T) {
	c, err := NewChain(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		in      string
		want    string
		dropped bool
	}{
		{"lowercase + stem", "Running", "run", false},
		{"stopword dropped", "the", "", true},
		{"contraction stopword dropped", "isn't", "", true},
		{"plain word stemmed", "connections", "connect", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := c.filter(tc.in)
			if tc.dropped {
				if ok {
					t.Fatalf("filter(%q) = %q, want dropped", tc.in, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("filter(%q) = %q/%v, want %q", tc.in, got, ok, tc.want)
			}
		})
	}
}

func TestChainUnknownConfig(t *testing.T) {
	if _, err := NewChain(Settings{Language: "xx", Filters: []string{"snowball"}}); err == nil {
		t.Fatal("expected error for unknown snowball language")
	}
	if _, err := NewChain(Settings{Language: "en", Filters: []string{"bogus"}}); err == nil {
		t.Fatal("expected error for unknown filter")
	}
}

func TestIndexSession(t *testing.T) {
	c, err := NewChain(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	s := c.NewIndexSession(func(tok string) error {
		out = append(out, tok)
		return nil
	})
	if err := s.Write([]byte("The Quick documents from Alice@Example.com")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// "The"/"from" are stopwords; the rest is lowercased and stemmed; the
	// whole address survives as one (lowercased) token.
	want := []string{"quick", "document", "alice@example.com", "alic", "exampl", "com"}
	if !sameElements(out, want) {
		t.Fatalf("index tokens = %q, want elements %q", out, want)
	}
}

func TestExpandSearch(t *testing.T) {
	c, err := NewChain(DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		query string
		want  [][]string // per-word expected variants (unordered)
	}{
		{
			name:  "single word variants",
			query: "Running",
			want:  [][]string{{"Running", "run"}},
		},
		{
			name:  "multi word AND with raw whole-string variant",
			query: "Foo Bar",
			want:  [][]string{{"Foo Bar", "Foo", "foo"}, {"Foo Bar", "Bar", "bar"}},
		},
		{
			name:  "stopword removed entirely",
			query: "the meeting",
			want:  [][]string{{"the meeting", "meeting", "meet"}},
		},
		{
			name:  "address searches as whole token",
			query: "alice@example.com",
			want:  [][]string{{"alice@example.com"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			words := c.ExpandSearch(tc.query)
			if len(words) != len(tc.want) {
				t.Fatalf("got %d words (%v), want %d", len(words), words, len(tc.want))
			}
			for i, w := range words {
				if !sameElements(w.Variants, tc.want[i]) {
					t.Fatalf("word %d variants = %q, want %q", i, w.Variants, tc.want[i])
				}
			}
		})
	}
}

func TestSettingsChecksum(t *testing.T) {
	c1, _ := NewChain(DefaultSettings())
	c2, _ := NewChain(DefaultSettings())
	if c1.SettingsChecksum() != c2.SettingsChecksum() {
		t.Fatal("same settings must give same checksum")
	}
	c3, _ := NewChain(Settings{Language: "en", Filters: []string{"lowercase"}})
	if c1.SettingsChecksum() == c3.SettingsChecksum() {
		t.Fatal("different filters must change the checksum")
	}
}
