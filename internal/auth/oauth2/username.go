package oauth2

import (
	"fmt"
	"strings"
)

// UsernameTemplate expands a small set of substitutions used by
// `username_validation_format`. The template is applied to the
// SASL authzid (the username the client claims to be) before
// comparing against the token's username claim. Default
// "%{user}" means identity (no transform).
//
// Substitutions:
//
//   - %u  / %{user}   — full username verbatim
//   - %Lu             — full username lowercased
//   - %n              — local part (before @)
//   - %Ln             — local part lowercased
//   - %d              — domain (after @); empty when no @
//   - %Ld             — domain lowercased
//
// The %L prefix is case-folding shorthand. Other transforms
// (%U upper, %E escape, %M md5, …) are intentionally omitted —
// add only when an operator hits a concrete need.
//
// An unknown substitution returns an error so a typo in
// configuration fails loudly at startup rather than silently
// producing wrong identities at runtime.
func UsernameTemplate(template, user string) (string, error) {
	if template == "" || template == "%{user}" || template == "%u" {
		return user, nil
	}
	var b strings.Builder
	b.Grow(len(template))
	for i := 0; i < len(template); i++ {
		c := template[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(template) {
			return "", fmt.Errorf("oauth2: trailing %% in template %q", template)
		}
		i++
		// %{user} long form.
		if template[i] == '{' {
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("oauth2: unterminated %%{ in template %q", template)
			}
			name := template[i+1 : i+end]
			i += end
			switch name {
			case "user":
				b.WriteString(user)
			default:
				return "", fmt.Errorf("oauth2: unknown %%{%s} in template %q", name, template)
			}
			continue
		}
		// %L… case-fold prefix.
		lower := false
		if template[i] == 'L' {
			lower = true
			if i+1 >= len(template) {
				return "", fmt.Errorf("oauth2: trailing %%L in template %q", template)
			}
			i++
		}
		switch template[i] {
		case 'u':
			if lower {
				b.WriteString(strings.ToLower(user))
			} else {
				b.WriteString(user)
			}
		case 'n':
			lp := localPart(user)
			if lower {
				lp = strings.ToLower(lp)
			}
			b.WriteString(lp)
		case 'd':
			dom := domainPart(user)
			if lower {
				dom = strings.ToLower(dom)
			}
			b.WriteString(dom)
		default:
			return "", fmt.Errorf("oauth2: unknown %%%c in template %q", template[i], template)
		}
	}
	return b.String(), nil
}

// localPart returns the segment before the first '@' in user,
// or the whole string when no '@' is present.
func localPart(user string) string {
	if at := strings.IndexByte(user, '@'); at >= 0 {
		return user[:at]
	}
	return user
}

// domainPart returns the segment after the first '@' in user, or
// an empty string when no '@' is present.
func domainPart(user string) string {
	if at := strings.IndexByte(user, '@'); at >= 0 {
		return user[at+1:]
	}
	return ""
}

// CompareUsername returns nil when the token's claimed username
// matches the templated SASL authzid. Returns ErrUsernameMismatch
// otherwise. Empty authzid (RFC 7628 allows it) skips the check —
// the validator's username claim becomes the resolved identity.
//
// Comparison is byte-exact after template expansion. Operators
// who need case-insensitive comparison use the %Lu template.
func CompareUsername(claimUsername, authzid, template string) (string, error) {
	if authzid == "" {
		return claimUsername, nil
	}
	expanded, err := UsernameTemplate(template, authzid)
	if err != nil {
		return "", err
	}
	if expanded != claimUsername {
		return "", fmt.Errorf("%w: token=%q authzid=%q (template %q → %q)",
			ErrUsernameMismatch, claimUsername, authzid, template, expanded)
	}
	return claimUsername, nil
}
