// Package threading decides which messages belong to one conversation.
//
// Two joins, in this order: by identity (References / In-Reply-To), which is
// what the sender told us and is safe; and by normalised subject, which is a
// guess and is where threading earns its reputation -- two strangers writing
// "Re: hello" must not become one conversation (#1425).
package threading

import "strings"

// replyPrefixes are the reply and forward markers stripped from a subject
// before it is used as a join key.
//
// Not English-only, deliberately: a deployment whose users write German or
// Polish would otherwise thread half their mail by identity and the other half
// not at all, which looks like the feature working badly rather than being
// absent. The list is the common set mail clients emit; anything not here
// simply fails to strip, which costs a join, not a wrong one.
var replyPrefixes = []string{
	"re", "aw", "sv", "vs", "odp", "antw", "ref", "res", "fwd", "fw", "wg", "tr", "rv", "enc", "vb", "doorst",
}

// NormalizeSubject reduces a subject to its join key: reply and forward
// markers removed, whitespace folded, case folded.
//
// It returns "" for a subject that is empty once stripped. An empty key never
// joins: a mailbox full of subject-less messages would otherwise become one
// enormous conversation, which is the failure mode of every naive
// implementation of this.
func NormalizeSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		stripped, ok := stripOnePrefix(s)
		if !ok {
			break
		}
		s = strings.TrimSpace(stripped)
	}
	// Trailing "(fwd)", the other half of the forward convention.
	for {
		lower := strings.ToLower(s)
		if !strings.HasSuffix(lower, "(fwd)") {
			break
		}
		s = strings.TrimSpace(s[:len(s)-len("(fwd)")])
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// stripOnePrefix removes one leading reply marker, with or without the
// bracketed counter clients add ("Re[2]:"), and reports whether it removed
// anything.
func stripOnePrefix(s string) (string, bool) {
	for _, p := range replyPrefixes {
		if len(s) < len(p) || !strings.EqualFold(s[:len(p)], p) {
			continue
		}
		rest := s[len(p):]
		// Re[2]: and Re(2): carry a depth counter.
		if len(rest) > 0 && (rest[0] == '[' || rest[0] == '(') {
			closer := byte(']')
			if rest[0] == '(' {
				closer = ')'
			}
			if end := strings.IndexByte(rest, closer); end > 0 && allDigits(rest[1:end]) {
				rest = rest[end+1:]
			}
		}
		if strings.HasPrefix(rest, ":") {
			return rest[1:], true
		}
	}
	return s, false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
