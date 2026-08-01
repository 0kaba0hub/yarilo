package ring

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"strings"
)

// HashFormat is a compiled username→hash-key template
// (director_service.username_hash). Supported: %u (whole user), %n
// (local part), %d (domain), each with an optional %L lowercase
// modifier, plus %% for a literal percent; anything else fails at parse
// time. The same HashFormat drives both the ring and the userDir hash.
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

// ParseHashFormat compiles a hash template; errors on an empty template
// or any unsupported variable. Validate once at startup.
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

// DefaultHashFormat is %Lu (whole username, lowercased), used when
// director_service.username_hash is unset.
func DefaultHashFormat() HashFormat {
	hf, _ := ParseHashFormat("%Lu")
	return hf
}

// MustParseHashFormat is ParseHashFormat that panics on error.
func MustParseHashFormat(format string) HashFormat {
	hf, err := ParseHashFormat(format)
	if err != nil {
		panic(err)
	}
	return hf
}

// Key expands the template into the string that gets hashed. For a
// username with no '@': %n is the whole username, %d is empty — so a %d
// template routes every domain-less user to the same backend.
// Intentional and deterministic.
func (f HashFormat) Key(username string) string {
	// fast path: a lone %u / %Lu / %n / ... — the common case
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
			v = username // %n of a domain-less user is the whole username
		}
	case fieldDomain:
		if at := strings.IndexByte(username, '@'); at >= 0 {
			v = username[at+1:]
		} else {
			v = "" // %d of a domain-less user is empty
		}
	}
	if s.lower {
		v = NormalizeUsername(v)
	}
	return v
}

// Hash folds a hash-key string into the uint32 ring hash: little-endian
// over the first 4 MD5 bytes. The single canonical folding for both the
// ring and the userDir; not byte-compatible with other implementations,
// only the routing semantics are shared.
func Hash(key string) uint32 {
	sum := md5.Sum([]byte(key))
	return binary.LittleEndian.Uint32(sum[:4])
}
