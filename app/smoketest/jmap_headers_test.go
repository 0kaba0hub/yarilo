package main

import "testing"

// The message this check appends must be what the wire carries. An APPEND
// literal declares a byte count, so a bare LF makes the message shorter on one
// side of the connection than the other — and the failure is not an error, it
// is an empty answer further along.
//
// Go does not rewrite line endings, which is exactly why this is asserted: the
// next person to edit the message with a raw string literal reintroduces it,
// and nothing else would notice until a rollout check failed somewhere
// unrelated.
func TestSmokeMessageIsCRLF(t *testing.T) {
	if err := assertCRLF(headerFormsMessage); err != nil {
		t.Error(err)
	}
}

func TestAssertCRLFRejectsABareLF(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		ok   bool
	}{
		{"crlf throughout", "Subject: a\r\n\r\nbody\r\n", true},
		{"a bare LF in the headers", "Subject: a\n\r\nbody\r\n", false},
		{"a bare LF in the body", "Subject: a\r\n\r\nbody\n", false},
		{"no line endings at all", "Subject: a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertCRLF(tc.msg)
			if tc.ok && err != nil {
				t.Errorf("rejected a CRLF message: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted a message with a bare LF; the literal length would not match the wire")
			}
		})
	}
}

// The expectations are compared as JSON documents, so key order does not decide
// whether a rollout passes — but the type must, since asURLs answering with the
// raw string instead of an array is the failure the form exists to catch.
func TestSameJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"whitespace", `{"a": 1}`, `{"a":1}`, true},
		{"a string is not an array", `"https://x"`, `["https://x"]`, false},
		{"null is not an empty array", `null`, `[]`, false},
		{"null is not an empty string", `null`, `""`, false},
		{"array order", `["a","b"]`, `["b","a"]`, false},
		{"nested", `[{"n":null,"e":"a@b"}]`, `[{"e":"a@b","n":null}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameJSON(tc.a, tc.b); got != tc.equal {
				t.Errorf("sameJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.equal)
			}
		})
	}
}
