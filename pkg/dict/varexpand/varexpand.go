// Package varexpand performs Dovecot-style %-variable substitution.
//
// Used by dict drivers that take filesystem paths or remote-key
// prefixes templated on user identity. The set of variables mirrors
// the subset of Dovecot's var-expand most commonly seen in
// dict_db / mail_attribute_dict config:
//
//	%u   full username  ("alice@example.com")
//	%n   local-part     ("alice")
//	%d   domain         ("example.com")  — empty when username has no '@'
//	%h   home directory
//	%i   numeric uid as text — empty when not supplied
//	%%   literal '%'
//
// Unknown verbs are passed through verbatim with the leading '%' kept,
// so a key like "abc%X/def" round-trips unchanged when "%X" is not
// recognised. This matches Dovecot's permissive expander, which logs
// a warning but does not fail the lookup.
package varexpand

import (
	"strings"
)

// Vars is the substitution table. Callers fill in whichever fields
// they have; missing values are treated as empty strings.
type Vars struct {
	Username string
	HomeDir  string
	UID      string
}

// Expand returns s with every recognised %verb replaced by the
// corresponding Vars field. Unknown verbs pass through with the '%'
// intact. A trailing '%' (no verb after it) is also passed through.
func Expand(s string, v Vars) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '%':
			b.WriteByte('%')
		case 'u':
			b.WriteString(v.Username)
		case 'n':
			b.WriteString(localPart(v.Username))
		case 'd':
			b.WriteString(domainPart(v.Username))
		case 'h':
			b.WriteString(v.HomeDir)
		case 'i':
			b.WriteString(v.UID)
		default:
			b.WriteByte('%')
			b.WriteByte(s[i+1])
		}
		i++
	}
	return b.String()
}

func localPart(user string) string {
	if at := strings.LastIndexByte(user, '@'); at >= 0 {
		return user[:at]
	}
	return user
}

func domainPart(user string) string {
	if at := strings.LastIndexByte(user, '@'); at >= 0 {
		return user[at+1:]
	}
	return ""
}
