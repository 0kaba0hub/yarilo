package oauth2

import (
	"fmt"
	"strings"
)

// UsernameTemplate expands the substitutions for `username_validation_format`,
// applied to the SASL authzid before comparing against the token's username claim.
// Default "%{user}" is identity. An unknown substitution returns an error so a
// config typo fails at startup rather than producing wrong identities.
//
// Substitutions (%L prefix lowercases):
//
//   - %u / %{user}    — full username verbatim
//   - %Lu             — full username lowercased
//   - %n              — local part (before @)
//   - %Ln             — local part lowercased
//   - %d              — domain (after @); empty when no @
//   - %Ld             — domain lowercased
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
		// %{name} long form.
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
		// %L… lowercase prefix.
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

// CompareUsername returns the resolved username when the token's claim matches the
// templated SASL authzid, else ErrUsernameMismatch. An empty authzid (allowed by
// RFC 7628) skips the check and adopts the claim. Comparison is byte-exact after
// expansion; use %Lu for case-insensitive matching.
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
