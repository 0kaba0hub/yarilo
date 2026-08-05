package mailbox

import (
	"fmt"
	"strings"
)

// Storage-name escaping keeps a client's mailbox name literal when the layout
// would otherwise reinterpret it.
//
// With namespace separator "/" over maildir++, "Invoices.2026" is written flat
// as ".Invoices.2026" and read back as two levels, so the client is handed
// "Invoices/2026" -- a mailbox it never created, silently substituted for the
// one it did. The name round-trips through SELECT, so only a client that LISTs
// finds out, and by then it has stored the wrong name (#1078).
//
// Escaping is what makes mailbox_list_refuse_layout_separator: false mean "the
// name keeps working" rather than "the name means something else". It is also
// the only half of the problem that fixes portability: with it on,
// "Invoices.2026" is one mailbox on maildir and on dbox alike, so a migration
// between formats preserves names.
//
// The escape set is deliberately selective rather than total. Escaping
// everything would be simpler and would also make every ordinary folder name
// unreadable on disk, which operators read.

// EscapeStorageName encodes the characters a layout would otherwise consume.
// name is already expressed in the layout separator: the caller maps the
// namespace separator to it first, so a "/" the client wrote as hierarchy has
// become layoutSep and is not escaped here, while a layoutSep the client wrote
// *literally* is.
//
// escape is a single character; empty disables escaping and returns the name
// unchanged, which is the default and keeps existing installations untouched.
func EscapeStorageName(name, layoutSep, escape string) string {
	if escape == "" || name == "" {
		return name
	}
	esc := escape[0]
	var sep byte
	if layoutSep != "" {
		sep = layoutSep[0]
	}

	var b strings.Builder
	b.Grow(len(name))
	atSegmentStart := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == esc:
			// The escape character itself, always: without this the reverse
			// pass cannot tell an escape from a literal and the round trip is
			// ambiguous rather than merely wrong.
			writeEscaped(&b, esc, c)
		case sep != 0 && c == sep:
			// A layout separator the client wrote literally. Hierarchy the
			// client asked for arrives here already as this byte too, but the
			// caller has split it out before calling: see FolderSubpath.
			writeEscaped(&b, esc, c)
		case c == '/':
			// Always: a "/" that is not the layout separator would still make
			// a directory level in every layout we write to.
			writeEscaped(&b, esc, c)
		case atSegmentStart && (c == '~' || c == '.'):
			// Only at the start of a path segment, where the filesystem gives
			// them meaning: "~" is a home reference to some tools, "." and
			// ".." are the traversal pair. "a.b" keeps its dot; ".hidden"
			// does not.
			writeEscaped(&b, esc, c)
		default:
			b.WriteByte(c)
		}
		atSegmentStart = c == '/' || (sep != 0 && c == sep)
	}
	return escapeReservedSegments(b.String(), esc, layoutSep)
}

func writeEscaped(b *strings.Builder, esc, c byte) {
	fmt.Fprintf(b, "%c%02x", esc, c)
}

// escapeReservedSegments hex-escapes the first byte of any segment that spells
// a name the layout owns. A folder called "cur" collides with maildir's own
// subdirectory, so it is stored as "^63ur" -- still readable, and no longer the
// same string the layout looks for.
func escapeReservedSegments(name string, esc byte, layoutSep string) string {
	seps := []string{"/"}
	if layoutSep != "" && layoutSep != "/" {
		seps = append(seps, layoutSep)
	}
	reserved := []string{"cur", "new", "tmp", dboxMailsSubdir}

	out := name
	for _, sep := range seps {
		parts := strings.Split(out, sep)
		for i, p := range parts {
			for _, r := range reserved {
				if strings.EqualFold(p, r) && p != "" {
					var b strings.Builder
					writeEscaped(&b, esc, p[0])
					b.WriteString(p[1:])
					parts[i] = b.String()
					break
				}
			}
		}
		out = strings.Join(parts, sep)
	}
	return out
}

// UnescapeStorageName reverses EscapeStorageName in one pass: an escape
// character followed by two hex digits is that byte, anything else is itself.
//
// A trailing escape character or a bad hex pair is left literal rather than
// reported: this runs over directory names found on disk, where a name that
// predates escaping, or one an operator created by hand, must still list.
func UnescapeStorageName(name, escape string) string {
	if escape == "" || name == "" {
		return name
	}
	esc := escape[0]
	if !strings.ContainsRune(name, rune(esc)) {
		return name
	}

	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		if name[i] != esc || i+2 >= len(name) {
			b.WriteByte(name[i])
			continue
		}
		hi, ok1 := unhex(name[i+1])
		lo, ok2 := unhex(name[i+2])
		if !ok1 || !ok2 {
			b.WriteByte(name[i])
			continue
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// EscapeLogicalName escapes a whole hierarchical name, segment by segment,
// before any encoding is applied to it.
//
// Order is the point. Escaping belongs at the logical-name boundary: escape,
// then encode on the way in; decode, then unescape on the way out. Applying it
// to an already-encoded name round-trips only while the two alphabets happen
// not to overlap -- which held for "^" and broke for an escape character that
// is a base64 digit, because modified-UTF-7 output is base64 and the reverse
// pass then cannot tell an escape from encoded text (#1078).
//
// nsSep is the separator the client speaks: hierarchy it asked for is preserved,
// and only what is inside a level is escaped.
func EscapeLogicalName(name, nsSep, layoutSep, escape string) string {
	if escape == "" || name == "" {
		return name
	}
	if nsSep == "" {
		nsSep = "/"
	}
	parts := strings.Split(name, nsSep)
	for i, p := range parts {
		parts[i] = EscapeStorageName(p, layoutSep, escape)
	}
	return strings.Join(parts, nsSep)
}
