// Package language is the tokenization and filter layer shared by FTS
// indexing and search. The generic tokenizer reproduces the reference
// "simple" algorithm — ASCII/Unicode word-break classes, apostrophe
// continuation, byte-length truncation and base64-run skipping — so indexes
// tokenize identically to ones built by the reference implementation
// (traceability in docs/FTS.md).
package language

import (
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultTokenMaxLen is the reference generic-tokenizer byte cap.
	DefaultTokenMaxLen = 30
	// DefaultAddressMaxLen mirrors the address tokenizer's maxlen.
	DefaultAddressMaxLen = 250
	// base64MinRun is the reference minimum base64 run length.
	base64MinRun = 50
)

// asciiWordBreaks is the reference ASCII break table: true = break. Apostrophe
// (0x27) is not a break — it gets the special continuation treatment below.
var asciiWordBreaks = [128]bool{}

func init() {
	for c := 0; c < 32; c++ {
		asciiWordBreaks[c] = true
	}
	for _, c := range " !\"#$%&()*+,-./:;<=>?@[\\]^`{|}~" {
		asciiWordBreaks[c] = true
	}
	asciiWordBreaks[127] = true
}

func isBreakRune(r rune) bool {
	if r < 0x80 {
		return asciiWordBreaks[r]
	}
	// Reference Unicode break rule: General Punctuation block plus the
	// whitespace / quotation / dash / terminal-punctuation classes.
	if r >= 0x2000 && r <= 0x206f {
		return true
	}
	return unicode.IsSpace(r) ||
		unicode.In(r, unicode.Quotation_Mark, unicode.Dash,
			unicode.Sentence_Terminal, unicode.Terminal_Punctuation)
}

func isApostrophe(r rune) bool {
	// U+2019 (right single quotation mark) and U+FF07 (fullwidth
	// apostrophe, #725 item 3) both get the same continuation treatment as
	// ASCII '\'' — normalized to U+0027 on append.
	return r == '\'' || r == 0x2019 || r == 0xFF07
}

var base64Class = [256]bool{}

func init() {
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/" {
		base64Class[c] = true
	}
}

func isBase64Leader(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '=', ';', ':', '?':
		return true
	}
	return false
}

// skipBase64 skips likely-base64 noise: from a token boundary, one or more
// runs of >= base64MinRun base64-alphabet bytes, each preceded by an allowed
// leader (or the buffer start) and followed by an allowed trailer (or the
// buffer end). Returns how many leading bytes of data are skippable.
func skipBase64(data []byte) int {
	start := 0
	for start < len(data) {
		first := start
		for first < len(data) && !base64Class[data[first]] {
			first++
		}
		if first > start && !isBase64Leader(data[first-1]) {
			break
		}
		past := first
		for past < len(data) && base64Class[data[past]] {
			past++
		}
		if past-first < base64MinRun {
			break
		}
		if past < len(data) && !isBase64Leader(data[past]) {
			break
		}
		start = past
	}
	return start
}

// EmitFunc receives each produced token. Returning an error aborts the feed.
type EmitFunc func(token string) error

// Generic is the streaming "simple" word tokenizer. Zero value is not ready;
// use NewGeneric.
type Generic struct {
	maxLen     int
	buf        []byte // token bytes, truncated at maxLen
	untrunc    int    // untruncated byte length of the current token
	prevLetter bool   // previous rune continued a word
	partial    []byte // split UTF-8 sequence across Feed boundaries
}

// NewGeneric returns a tokenizer with the given byte-length cap
// (0 = DefaultTokenMaxLen).
func NewGeneric(maxLen int) *Generic {
	if maxLen <= 0 {
		maxLen = DefaultTokenMaxLen
	}
	return &Generic{maxLen: maxLen}
}

func (g *Generic) appendTruncated(b []byte) {
	if room := g.maxLen - len(g.buf); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		g.buf = append(g.buf, b...)
	}
	g.untrunc += len(b)
}

func (g *Generic) emitCurrent(emit EmitFunc) error {
	b := g.buf
	if g.untrunc <= g.maxLen {
		// Trailing apostrophe is dropped unless the token was truncated.
		if n := len(b); n > 0 && b[n-1] == '\'' {
			b = b[:n-1]
		}
	} else {
		b = trimPartialRune(b)
	}
	g.buf = g.buf[:0]
	g.untrunc = 0
	g.prevLetter = false
	if len(b) == 0 {
		return nil
	}
	return emit(string(b))
}

func trimPartialRune(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// Feed streams data through the tokenizer. Tokens are emitted as soon as a
// break is seen; a token spanning Feed boundaries is held until completed.
func (g *Generic) Feed(data []byte, emit EmitFunc) error {
	if len(g.partial) > 0 {
		data = append(g.partial, data...)
		g.partial = nil
	}
	i := 0
	for i < len(data) {
		if len(g.buf) == 0 && g.untrunc == 0 {
			i += skipBase64(data[i:])
			if i >= len(data) {
				break
			}
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(data[i:]) {
				// Split sequence: keep for the next Feed.
				g.partial = append(g.partial, data[i:]...)
				return nil
			}
			// Genuinely invalid byte: treat as a break.
			if err := g.emitCurrent(emit); err != nil {
				return err
			}
			i++
			continue
		}
		switch {
		case isApostrophe(r):
			// A letter followed by an apostrophe continues the word; an
			// apostrophe anywhere else (start, doubled) is a break.
			if g.prevLetter {
				g.appendTruncated([]byte{'\''})
				g.prevLetter = false
			} else if err := g.emitCurrent(emit); err != nil {
				return err
			}
		case isBreakRune(r):
			if err := g.emitCurrent(emit); err != nil {
				return err
			}
		default:
			g.appendTruncated(data[i : i+size])
			g.prevLetter = true
		}
		i += size
	}
	return nil
}

// Flush emits any pending token at end of input.
func (g *Generic) Flush(emit EmitFunc) error {
	g.partial = nil
	return g.emitCurrent(emit)
}
