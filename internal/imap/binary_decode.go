package imap

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"
)

// decodeBinarySection implements the RFC 3516 BINARY[<section>] decoding —
// strip the Content-Transfer-Encoding wrapper and return the raw bytes.
//
// Whole-message (part == nil or empty) is fully supported: the message
// header is scanned for Content-Transfer-Encoding, base64 / quoted-
// printable bodies are decoded, 7bit / 8bit / binary pass through.
//
// Part-spec (BINARY[1] / BINARY[1.2] etc.) requires a MIME walk over the
// message structure. Until that lands the call returns the raw bytes
// unchanged — clients that supply a part still get a syntactically valid
// reply rather than an error (permissive fallback when the MIME parser
// cannot resolve the section).
func decodeBinarySection(raw []byte, part []int) []byte {
	if len(part) > 0 {
		// MIME walk deferred; return body unchanged so the client can
		// fall back to its own decoder.
		return raw
	}
	header, body, ok := splitMessage(raw)
	if !ok {
		return raw
	}
	enc := strings.ToLower(strings.TrimSpace(headerValue(header, "Content-Transfer-Encoding")))
	switch enc {
	case "", "7bit", "8bit", "binary":
		return body
	case "base64":
		// Strip CRLF / whitespace before decoding — RFC 2045 §6.8 allows
		// line breaks anywhere in the encoded data.
		clean := stripWhitespace(body)
		decoded, err := base64.StdEncoding.DecodeString(string(clean))
		if err != nil {
			return body
		}
		return decoded
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return body
		}
		return decoded
	}
	return body
}

// splitMessage finds the blank line that separates the RFC 5322 header
// block from the body. Returns (header, body, true) on success or the
// whole input as header with empty body and false when no separator is
// present.
func splitMessage(raw []byte) ([]byte, []byte, bool) {
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == '\n' {
			// CRLF-CRLF or LF-LF separator.
			if raw[i+1] == '\n' {
				return raw[:i+1], raw[i+2:], true
			}
			if i+3 < len(raw) && raw[i+1] == '\r' && raw[i+2] == '\n' {
				return raw[:i+1], raw[i+3:], true
			}
		}
	}
	return raw, nil, false
}

// headerValue returns the (last) value of name in the supplied header
// block, RFC 5322 unfold-aware. Comparison is case-insensitive per spec.
func headerValue(header []byte, name string) string {
	want := strings.ToLower(name)
	var current string
	for _, line := range bytes.Split(header, []byte{'\n'}) {
		l := strings.TrimRight(string(line), "\r")
		if l == "" {
			continue
		}
		// Continuation line — append to the current value.
		if l[0] == ' ' || l[0] == '\t' {
			current += " " + strings.TrimSpace(l)
			continue
		}
		colon := strings.IndexByte(l, ':')
		if colon < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(l[:colon])) == want {
			current = strings.TrimSpace(l[colon+1:])
		}
	}
	return current
}

// stripWhitespace removes ASCII whitespace from b, used to undo line
// folding inside base64-encoded bodies.
func stripWhitespace(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		out = append(out, c)
	}
	return out
}
