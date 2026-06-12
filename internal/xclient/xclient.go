// Package xclient implements the XCLIENT SMTP extension (Postfix-compatible).
// Wire format: XCLIENT key=xtext-value [key=xtext-value ...]
// xtext: unreserved chars 0-9 A-Z a-z - _ pass through; others as +XX (uppercase hex).
// [UNAVAILABLE] is the canonical "not set" sentinel per RFC / Postfix convention.
package xclient

import (
	"fmt"
	"strings"
)

// Attrs holds the fields carried by one or more XCLIENT commands.
type Attrs struct {
	Proto    string // SMTP | ESMTP | LMTP
	Addr     string // client IP
	Port     string // client port
	Helo     string // client HELO/EHLO domain
	Login    string // authenticated login name (SMTP compat: LOGIN=)
	User     string // authenticated username in login→backend preamble (USER=)
	Token    string // one-time session token from yarilo-auth (TOKEN=)
	Session  string // session ID
	TTL      string // hop count remaining
	Forward  string // base64-encoded forward data
	DestAddr string // destination IP the client originally connected to (Dovecot 2.4 LMTP: DESTADDR; login-proxy: DESTIP)
	DestPort string // destination port the client originally connected to
}

const unavailable = "[UNAVAILABLE]"

// maxLineLen is the Postfix/Dovecot limit per XCLIENT command (INTERNALS.md §22).
const maxLineLen = 512

// DecodeXText decodes an xtext-encoded value.
// +XX sequences are converted to their byte values.
// The sentinel [UNAVAILABLE] decodes to an empty string.
func DecodeXText(s string) (string, error) {
	if s == unavailable {
		return "", nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '+' {
			if i+2 >= len(s) {
				return "", fmt.Errorf("xclient/xtext: truncated +XX at position %d", i)
			}
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if !ok1 || !ok2 {
				return "", fmt.Errorf("xclient/xtext: invalid hex %q at position %d", s[i:i+3], i)
			}
			b.WriteByte(hi<<4 | lo)
			i += 3
		} else {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), nil
}

// EncodeXText encodes a string as xtext.
// Empty string encodes to [UNAVAILABLE].
// Unreserved chars (0-9 A-Z a-z - _) pass through; all others become +XX.
func EncodeXText(s string) string {
	if s == "" {
		return unavailable
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('+')
			b.WriteByte(hexDigit[c>>4])
			b.WriteByte(hexDigit[c&0xf])
		}
	}
	return b.String()
}

// Parse parses a single XCLIENT command line (with or without "XCLIENT " prefix).
// Unknown keys are silently ignored. Values are xtext-decoded.
func Parse(line string) (Attrs, error) {
	line = strings.TrimPrefix(line, "XCLIENT ")
	line = strings.TrimPrefix(line, "xclient ")
	var a Attrs
	for _, kv := range strings.Fields(line) {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(kv[:eq])
		val, err := DecodeXText(kv[eq+1:])
		if err != nil {
			return Attrs{}, fmt.Errorf("xclient/parse: key %s: %w", key, err)
		}
		switch key {
		case "PROTO":
			a.Proto = val
		case "ADDR":
			a.Addr = val
		case "PORT":
			a.Port = val
		case "HELO":
			a.Helo = val
		case "LOGIN":
			a.Login = val
		case "USER":
			a.User = val
		case "TOKEN":
			a.Token = val
		case "SESSION":
			a.Session = val
		case "TTL":
			a.TTL = val
		case "FORWARD":
			a.Forward = val
		case "DESTADDR", "DESTIP":
			a.DestAddr = val
		case "DESTPORT":
			a.DestPort = val
		}
	}
	return a, nil
}

// Format encodes Attrs as one or more XCLIENT command lines.
// Lines are split when they would exceed maxLineLen bytes.
// Empty fields are omitted.
func Format(a Attrs) []string {
	pairs := []struct{ k, v string }{
		{"PROTO", a.Proto},
		{"ADDR", a.Addr},
		{"PORT", a.Port},
		{"HELO", a.Helo},
		{"LOGIN", a.Login},
		{"USER", a.User},
		{"TOKEN", a.Token},
		{"SESSION", a.Session},
		{"TTL", a.TTL},
		{"FORWARD", a.Forward},
		{"DESTADDR", a.DestAddr},
		{"DESTPORT", a.DestPort},
	}

	var lines []string
	cur := "XCLIENT"
	for _, p := range pairs {
		if p.v == "" {
			continue
		}
		token := " " + p.k + "=" + EncodeXText(p.v)
		if len(cur)+len(token) > maxLineLen && cur != "XCLIENT" {
			lines = append(lines, cur)
			cur = "XCLIENT"
		}
		cur += token
	}
	if cur != "XCLIENT" {
		lines = append(lines, cur)
	}
	return lines
}

const hexDigit = "0123456789ABCDEF"

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func isUnreserved(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') || c == '-' || c == '_'
}
