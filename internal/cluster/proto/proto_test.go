package proto

import "testing"

var tabEscapeTests = []struct {
	input string
	want  string
}{
	{"hello", "hello"},
	{"a\tb", `a\tb`},
	{"a\nb", `a\nb`},
	{"a\rb", `a\rb`},
	{`a\b`, `a\\b`},
	{"a\t\nb", `a\t\nb`},
}

func TestTabEscape(t *testing.T) {
	for _, tc := range tabEscapeTests {
		got := TabEscape(tc.input)
		if got != tc.want {
			t.Errorf("TabEscape(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTabUnescape(t *testing.T) {
	for _, tc := range tabEscapeTests {
		got := TabUnescape(tc.want)
		if got != tc.input {
			t.Errorf("TabUnescape(%q) = %q, want %q", tc.want, got, tc.input)
		}
	}
}

func TestTabRoundtrip(t *testing.T) {
	cases := []string{
		"user@example.com",
		"value with\ttab",
		"value with\nnewline",
		`back\slash`,
		"",
		"a\t\n\r\\b",
	}
	for _, s := range cases {
		if got := TabUnescape(TabEscape(s)); got != s {
			t.Errorf("roundtrip(%q) = %q", s, got)
		}
	}
}

func TestParseLine(t *testing.T) {
	fields := ParseLine("VERSION\tyarilo-director\t1\t0")
	want := []string{"VERSION", "yarilo-director", "1", "0"}
	if len(fields) != len(want) {
		t.Fatalf("len %d, want %d", len(fields), len(want))
	}
	for i, f := range fields {
		if f != want[i] {
			t.Errorf("[%d] got %q, want %q", i, f, want[i])
		}
	}
}
