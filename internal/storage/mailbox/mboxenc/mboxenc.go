// Package mboxenc implements folder name encoding helpers shared across
// all mailbox storage backends (maildir, sdbox, mdbox).
package mboxenc

import (
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ToModUTF7 encodes a UTF-8 folder name to modified-UTF-7 (RFC 3501 §5.1.3)
// for storage on a filesystem that was originally populated by a legacy server
// using that encoding. Printable ASCII except '&' passes through unchanged.
// '&' becomes "&-". Non-printable or non-ASCII code points are encoded as
// &<modified-base64 of UTF-16BE>-.
func ToModUTF7(s string) string {
	runes := []rune(s)
	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(runes) {
		r := runes[i]
		if r == '&' {
			buf.WriteString("&-")
			i++
			continue
		}
		if r >= 0x20 && r <= 0x7E {
			buf.WriteRune(r)
			i++
			continue
		}
		// Collect run of non-printable-ASCII runes.
		j := i
		for j < len(runes) {
			c := runes[j]
			if c >= 0x20 && c <= 0x7E && c != '&' {
				break
			}
			j++
		}
		// Encode as UTF-16BE then modified-base64.
		var utf16be []byte
		for _, c := range runes[i:j] {
			if c <= 0xFFFF {
				utf16be = append(utf16be, byte(c>>8), byte(c))
			} else {
				// Surrogate pair for codepoints above U+FFFF.
				c -= 0x10000
				hi := uint16(0xD800 + (c >> 10))
				lo := uint16(0xDC00 + (c & 0x3FF))
				utf16be = append(utf16be, byte(hi>>8), byte(hi), byte(lo>>8), byte(lo))
			}
		}
		// Modified base64: '/' replaced with ','. No padding.
		encoded := base64.RawStdEncoding.EncodeToString(utf16be)
		encoded = strings.ReplaceAll(encoded, "/", ",")
		buf.WriteByte('&')
		buf.WriteString(encoded)
		buf.WriteByte('-')
		i = j
	}
	return buf.String()
}

// FromModUTF7 decodes a modified-UTF-7 folder name (RFC 3501 §5.1.3) to UTF-8.
// Returns an error if the encoding is malformed.
func FromModUTF7(s string) (string, error) {
	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '&' {
			buf.WriteByte(s[i])
			i++
			continue
		}
		// Find closing '-'.
		j := i + 1
		for j < len(s) && s[j] != '-' {
			j++
		}
		if j >= len(s) {
			return "", fmt.Errorf("mboxenc: unterminated modified-UTF-7 sequence in %q", s)
		}
		if j == i+1 {
			// "&-" is the literal '&'.
			buf.WriteByte('&')
			i = j + 1
			continue
		}
		// Decode modified-base64: ',' → '/'.
		encoded := strings.ReplaceAll(s[i+1:j], ",", "/")
		// base64.RawStdEncoding handles unpadded input.
		decoded, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("mboxenc: base64 decode %q: %w", s[i+1:j], err)
		}
		if len(decoded)%2 != 0 {
			return "", fmt.Errorf("mboxenc: odd UTF-16BE byte count in %q", s)
		}
		// Decode UTF-16BE to UTF-8.
		for k := 0; k < len(decoded); k += 2 {
			u := uint16(decoded[k])<<8 | uint16(decoded[k+1])
			if u >= 0xD800 && u <= 0xDBFF {
				// High surrogate — expect low surrogate next.
				if k+4 > len(decoded) {
					return "", fmt.Errorf("mboxenc: truncated surrogate pair in %q", s)
				}
				lo := uint16(decoded[k+2])<<8 | uint16(decoded[k+3])
				if lo < 0xDC00 || lo > 0xDFFF {
					return "", fmt.Errorf("mboxenc: invalid low surrogate %04X in %q", lo, s)
				}
				r := rune(0x10000 + (rune(u-0xD800) << 10) + rune(lo-0xDC00))
				buf.WriteRune(r)
				k += 2
			} else {
				buf.WriteRune(rune(u))
			}
		}
		i = j + 1
	}
	return buf.String(), nil
}

// NFC returns the NFC form of s (no-op for already-NFC input).
func NFC(s string) string {
	return norm.NFC.String(s)
}
