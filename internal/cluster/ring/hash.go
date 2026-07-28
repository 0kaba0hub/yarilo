package ring

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"strings"
)

// HashFormat is a compiled username→hash-key template (director_service.username_hash,
// #850). It mirrors the reference director's director_username_hash expression so an
// operator can copy that value verbatim, but supports only the variables that actually
// change routing — %u (whole user), %n (local part, up to the first '@'), %d (domain,
// after the first '@'), each with an optional %L lowercase modifier — plus %% for a
// literal percent. Anything else is rejected loudly at parse time rather than silently
// mis-routing. The general var-expand engine is deliberately NOT pulled in: these are
// the only expressions a hash template realistically uses.
//
// The SAME HashFormat is used to derive the hash on both the ring and the director
// userDir side; there is one Key implementation, so the two can never drift apart — the
// invariant is structural, not a matched pair of hand-copied functions.
//
// The zero value is invalid; build one with ParseHashFormat.
type HashFormat struct {
	segs []hashSeg
	raw  string
}

// hashField selects which slice of the username a segment contributes.
type hashField int

const (
	fieldLiteral hashField = iota // seg.lit is emitted verbatim (covers %% → %)
	fieldUser                     // %u — whole username
	fieldLocal                    // %n — local part (before first '@'; whole username if none)
	fieldDomain                   // %d — domain (after first '@'; empty if none)
)

type hashSeg struct {
	field hashField
	lit   string // only for fieldLiteral
	lower bool   // %L modifier: lowercase this segment's value
}

// ParseHashFormat compiles a hash template, returning an error for an empty template
// or any unsupported variable. Callers validate ONCE at startup so the hot hashing
// path never has to.
func ParseHashFormat(format string) (HashFormat, error) {
	if format == "" {
		return HashFormat{}, fmt.Errorf("ring: empty username hash format")
	}
	var (
		segs []hashSeg
		lit  strings.Builder
	)
	flushLit := func() {
		if lit.Len() > 0 {
			segs = append(segs, hashSeg{field: fieldLiteral, lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			lit.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) {
			return HashFormat{}, fmt.Errorf("ring: username hash format %q: dangling %% at end", format)
		}
		lower := false
		if format[i] == 'L' {
			lower = true
			i++
			if i >= len(format) {
				return HashFormat{}, fmt.Errorf("ring: username hash format %q: dangling %%L at end", format)
			}
		}
		switch format[i] {
		case '%':
			if lower {
				return HashFormat{}, fmt.Errorf("ring: username hash format %q: %%L%% is not valid (lowercase modifier on a literal percent)", format)
			}
			lit.WriteByte('%') // %% → literal percent
		case 'u':
			flushLit()
			segs = append(segs, hashSeg{field: fieldUser, lower: lower})
		case 'n':
			flushLit()
			segs = append(segs, hashSeg{field: fieldLocal, lower: lower})
		case 'd':
			flushLit()
			segs = append(segs, hashSeg{field: fieldDomain, lower: lower})
		default:
			return HashFormat{}, fmt.Errorf("ring: username hash format %q: unsupported variable %%%c — only %%u, %%n, %%d (with optional %%L) and %%%% are allowed", format, format[i])
		}
	}
	flushLit()
	return HashFormat{segs: segs, raw: format}, nil
}

// String returns the original template, for logging/config echo.
func (f HashFormat) String() string { return f.raw }

// DefaultHashFormat is the %Lu template — the historical default (#738): the whole
// username lowercased. Used when director_service.username_hash is unset.
func DefaultHashFormat() HashFormat {
	hf, _ := ParseHashFormat("%Lu")
	return hf
}

// MustParseHashFormat is ParseHashFormat that panics on error, for tests and static
// call sites with a known-good literal template.
func MustParseHashFormat(format string) HashFormat {
	hf, err := ParseHashFormat(format)
	if err != nil {
		panic(err)
	}
	return hf
}

// Key expands the template for username into the string that gets hashed. A username
// with no '@' follows the reference semantics: %n is the whole username, %d is empty —
// so a %d template routes every domain-less user to the same key (and thus the same
// backend). That is intentional and deterministic; it is documented and tested so the
// first local (domain-less) account in a deployment does not look like a bug.
func (f HashFormat) Key(username string) string {
	// Fast path: a lone %u / %Lu / %n / ... — the overwhelmingly common case.
	if len(f.segs) == 1 && f.segs[0].field != fieldLiteral {
		return fieldValue(username, f.segs[0])
	}
	var b strings.Builder
	for _, s := range f.segs {
		if s.field == fieldLiteral {
			b.WriteString(s.lit)
			continue
		}
		b.WriteString(fieldValue(username, s))
	}
	return b.String()
}

func fieldValue(username string, s hashSeg) string {
	var v string
	switch s.field {
	case fieldUser:
		v = username
	case fieldLocal:
		if at := strings.IndexByte(username, '@'); at >= 0 {
			v = username[:at]
		} else {
			v = username // %n of a domain-less user is the whole username (reference t_strcut)
		}
	case fieldDomain:
		if at := strings.IndexByte(username, '@'); at >= 0 {
			v = username[at+1:]
		} else {
			v = "" // %d of a domain-less user is empty (reference i_strchr_to_next)
		}
	}
	if s.lower {
		v = NormalizeUsername(v)
	}
	return v
}

// Hash folds a hash-key string into the uint32 ring hash. This is the single canonical
// folding used by BOTH the ring and the director userDir. Note: this is little-endian
// over the first 4 MD5 bytes (yarilo's own choice since #738) — deliberately NOT the
// reference director's big-endian fold + 0→1 remap. Those bytes only matter for
// byte-compatibility with a live reference-director ring running alongside ours, a
// scenario our architecture never produces (yarilo replaces the director wholesale);
// we borrow the routing SEMANTICS, not the byte artifacts.
func Hash(key string) uint32 {
	sum := md5.Sum([]byte(key))
	return binary.LittleEndian.Uint32(sum[:4])
}
