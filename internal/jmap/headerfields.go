package jmap

import (
	"strings"
	"time"

	"net/mail"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// headerFieldValues answers the request's header:* properties from the parsed
// header block (RFC 8621 §4.1.3).
//
// The map is keyed by the property as the client wrote it, because that is the
// key the answer must come back under: header:To:asText and header:To:asRaw are
// different properties of the same field.
func headerFieldValues(h message.Header, props []jmapcore.HeaderProperty) map[string]any {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]any, len(props))
	for _, p := range props {
		raw := rawHeaderValues(h, p.Name)
		if p.All {
			// An array, empty when the field is absent — "all of them" is a
			// complete answer even when there are none, unlike a single value
			// which has to be null to say the field was not there.
			values := make([]any, 0, len(raw))
			for _, v := range raw {
				values = append(values, headerFormValue(v, p.Form))
			}
			out[p.Property] = values
			continue
		}
		if len(raw) == 0 {
			out[p.Property] = nil
			continue
		}
		// The last occurrence: a header added by the most recent handler is the
		// one that describes the message as it arrived here.
		out[p.Property] = headerFormValue(raw[len(raw)-1], p.Form)
	}
	return out
}

// allHeaders lists every field in the order the message carries them.
//
// The order is the point: Received lines read as a route, and a set of fields
// sorted or deduplicated is a different message from the one that arrived.
func allHeaders(h message.Header) []jmapcore.EmailHeader {
	out := []jmapcore.EmailHeader{}
	fields := h.Fields()
	for fields.Next() {
		raw, err := fields.Raw()
		if err != nil {
			out = append(out, jmapcore.EmailHeader{Name: fields.Key(), Value: fields.Value()})
			continue
		}
		name, value, found := strings.Cut(string(raw), ":")
		if !found {
			continue
		}
		out = append(out, jmapcore.EmailHeader{
			Name:  strings.TrimSpace(name),
			Value: strings.TrimRight(value, "\r\n"),
		})
	}
	return out
}

// rawHeaderValues collects every occurrence of a field, in the order they
// appear in the message.
func rawHeaderValues(h message.Header, name string) []string {
	var out []string
	fields := h.FieldsByKey(name)
	for fields.Next() {
		// The raw form keeps the value as it was written, minus the CRLF that
		// ends it; folding whitespace is part of the raw value (§4.1.3).
		v, err := fields.Raw()
		if err != nil {
			out = append(out, fields.Value())
			continue
		}
		_, value, found := strings.Cut(string(v), ":")
		if !found {
			continue
		}
		out = append(out, strings.TrimRight(value, "\r\n"))
	}
	// FieldsByKey walks from the top of the header, which is the order the
	// message carries.
	return out
}

// headerFormValue renders one raw value in the requested form.
func headerFormValue(raw string, form jmapcore.HeaderForm) any {
	switch form {
	case jmapcore.FormText:
		// Leading and trailing whitespace goes: the space after the colon is
		// framing, not part of the value (§4.1.3).
		return decodeWord(strings.TrimSpace(unfold(raw)))
	case jmapcore.FormAddresses:
		addrs := addresses(strings.TrimSpace(unfold(raw)))
		if addrs == nil {
			return []jmapcore.EmailAddress{}
		}
		return addrs
	case jmapcore.FormGroupedAddresses:
		return groupedAddresses(unfold(raw))
	case jmapcore.FormMessageIDs:
		ids := messageIDs(unfold(raw))
		if ids == nil {
			return nil
		}
		return ids
	case jmapcore.FormDate:
		t, err := mail.ParseDate(strings.TrimSpace(unfold(raw)))
		if err != nil {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	case jmapcore.FormURLs:
		return urlsOf(raw)
	default: // FormRaw
		return raw
	}
}

// unfold joins the continuation lines of a folded header into one line.
//
// Unfolding is the removal of the CRLF and nothing else (RFC 5322 §2.2.3):
// folding inserts a CRLF *before* whitespace that was already there, so the
// space that follows a continuation is part of the value and reappears on its
// own. Writing one for the newline as well produced two spaces at every fold,
// which the address parsers tolerated and asText -- the form a client
// displays -- did not hide.
func unfold(raw string) string {
	if !strings.ContainsAny(raw, "\r\n") {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\r', '\n':
		default:
			b.WriteByte(raw[i])
		}
	}
	return b.String()
}

// urlsOf reads the angle-bracketed URLs of a List-* field (§4.1.3). Anything
// outside the brackets is commentary and is not a URL.
func urlsOf(raw string) []string {
	var out []string
	rest := unfold(raw)
	for {
		open := strings.Index(rest, "<")
		if open < 0 {
			break
		}
		close := strings.Index(rest[open:], ">")
		if close < 0 {
			break
		}
		if url := strings.TrimSpace(rest[open+1 : open+close]); url != "" {
			out = append(out, url)
		}
		rest = rest[open+close+1:]
	}
	if out == nil {
		return []string{}
	}
	return out
}

// groupedAddresses splits an address field into its RFC 5322 groups.
//
// Addresses outside any group are returned under a null name, which is what
// §4.1.3 asks for, and a field with no groups at all therefore answers as one
// unnamed group. The split is on the top-level ':' and ';' only — a colon
// inside a quoted display name does not start a group.
func groupedAddresses(v string) []jmapcore.EmailAddressGroup {
	v = strings.TrimSpace(v)
	if v == "" {
		return []jmapcore.EmailAddressGroup{}
	}
	var (
		out     []jmapcore.EmailAddressGroup
		quoted  bool
		escaped bool
		depth   int
		segment strings.Builder
		name    *string
	)
	flush := func() {
		text := strings.Trim(strings.TrimSpace(segment.String()), ",")
		segment.Reset()
		if text == "" && name == nil {
			return
		}
		out = append(out, jmapcore.EmailAddressGroup{Name: name, Addresses: addressesOrEmpty(text)})
		name = nil
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case escaped:
			// A quoted-pair: the backslash quoted whatever follows, so it is
			// content and cannot open or close anything. Without this a display
			// name containing an escaped quote flips the quoting state, and the
			// group then splits on a ';' that is inside a string (RFC 5322
			// §3.2.1).
			//
			// Both bytes are kept, not just the quoted one: what leaves here is
			// handed to an address parser, and "say "hi"" is no longer an
			// address list. Dropping the backslash moved the failure rather
			// than removing it — the split became right and the value became
			// unparseable.
			escaped = false
			if quoted {
				segment.WriteByte('\\')
				segment.WriteByte(c)
			}
		case c == '\\' && (quoted || depth > 0):
			escaped = true
		case c == '"':
			quoted = !quoted
			segment.WriteByte(c)
		case quoted:
			segment.WriteByte(c)
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		case depth > 0:
		case c == ':':
			// Only the last token before the colon is the group's name;
			// anything before that belongs to the ungrouped addresses that
			// preceded it. A list may mix the two — "a@x, Team:b@x;, c@x" is
			// three groups, not one — and taking the whole segment as the name
			// swallowed the addresses in front of it.
			pending := segment.String()
			segment.Reset()
			label := pending
			if comma := strings.LastIndex(pending, ","); comma >= 0 {
				segment.WriteString(pending[:comma])
				label = pending[comma+1:]
				flush()
			}
			if label = strings.TrimSpace(label); label != "" {
				n := label
				name = &n
			}
		case c == ';':
			flush()
		default:
			segment.WriteByte(c)
		}
	}
	flush()
	if out == nil {
		return []jmapcore.EmailAddressGroup{}
	}
	return out
}

func addressesOrEmpty(v string) []jmapcore.EmailAddress {
	if a := addresses(v); a != nil {
		return a
	}
	return []jmapcore.EmailAddress{}
}
