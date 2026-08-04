package jmapcore

import (
	"fmt"
	"sort"
	"strings"
)

// UnknownProperties returns the requested names that value does not carry, in
// the order the client named them.
//
// A header field property is valid when it parses (§4.1.3); anything else must
// be a JSON property of the object.
func UnknownProperties(value any, props []string) []string {
	if props == nil {
		return nil
	}
	known := jsonFields(structTypeOf(value))
	var out []string
	for _, p := range props {
		if IsHeaderProperty(p) {
			if _, ok := ParseHeaderProperty(p); !ok {
				out = append(out, p)
			}
			continue
		}
		if _, ok := known[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// InvalidProperties builds the error a request with unknown property names
// must be answered with (RFC 8620 §5.1).
//
// The whole call is refused rather than the names ignored. Ignoring them is
// what this replaces, and it fails in the worst way available: the client
// receives a successful response with the property absent, and cannot tell a
// typo from a property the server has not implemented — which is exactly the
// pair it needs to tell apart.
func InvalidProperties(unknown []string) *MethodError {
	quoted := make([]string, 0, len(unknown))
	for _, p := range unknown {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	// Named in full: a client that sent twenty properties and one typo should
	// not have to bisect them.
	return &MethodError{
		Type:        ErrInvalidArguments,
		Description: "unknown properties: " + strings.Join(quoted, ", "),
		Arguments:   []string{"properties"},
	}
}

// KnownProperties lists the property names an object carries, sorted. For
// tests and for an error message that has to suggest something.
func KnownProperties(value any) []string {
	fields := jsonFields(structTypeOf(value))
	out := make([]string, 0, len(fields))
	for name := range fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
