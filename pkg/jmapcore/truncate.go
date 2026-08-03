package jmapcore

import (
	"strings"
	"unicode/utf8"
)

// TruncateBody cuts a decoded body value to at most limit bytes and reports
// whether anything was removed. RFC 8621 §4.2.2 forbids splitting a multi-octet
// character, so the cut moves back to a rune boundary; a limit of 0 means no
// limit.
func TruncateBody(s string, limit uint32) (string, bool) {
	if limit == 0 || uint32(len(s)) <= limit {
		return s, false
	}
	cut := int(limit)
	// Walk back off any continuation bytes so the result is still valid UTF-8.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// A rune that starts before the cut but ends after it must go entirely.
	if cut > 0 {
		if r, size := utf8.DecodeLastRuneInString(s[:cut]); r == utf8.RuneError && size <= 1 {
			cut--
		}
	}
	return s[:cut], true
}

// TruncateHTML cuts an HTML body value like TruncateBody, then drops a trailing
// partial tag. Cutting mid-tag would leave markup a client renders as text or,
// worse, as an unterminated element swallowing the rest of the document.
func TruncateHTML(s string, limit uint32) (string, bool) {
	out, truncated := TruncateBody(s, limit)
	if !truncated {
		return out, false
	}
	if open := strings.LastIndexByte(out, '<'); open > strings.LastIndexByte(out, '>') {
		out = out[:open]
	}
	return out, true
}
