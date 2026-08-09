package mailbox

import (
	"crypto/md5"
	"crypto/sha1"
	"fmt"
	"strconv"
	"strings"
)

// Path templates come in two dialects and this file implements both.
//
// The expression form is a variable followed by filters:
//
//	%{user}                            alice@example.com
//	%{user | username}                 alice
//	%{user | domain}                   example.com
//	%{home}                            the user's home directory
//	%{user | sha1 % 256 | hex(2)}      "3f" — a hash bucket
//
// The short form (%u, %n, %d, %h, %2.256Nu) predates it and stays accepted:
// it is in every values file, every example and in configs written before this.
//
// A sequence that belongs to neither is an error. It used to pass through
// verbatim, which meant a template copied from a newer reference config
// produced a directory literally named "%{user | sha1 % 256 | hex(2)}" —
// created, used, and never complained about. A path we cannot expand is a
// misconfiguration, and it has to say so.

// TemplateVars are the values a path template can name.
type TemplateVars struct {
	User string
	Home string
}

// ExpandTemplate expands both dialects, or reports the sequence it did not
// understand.
func ExpandTemplate(tmpl string, vars TemplateVars) (string, error) {
	return expandTemplate(tmpl, vars, false)
}

// expandTemplate carries one switch: keepHome leaves the home variable in place
// instead of expanding it. ExpandVars is called from sites that substitute the
// home themselves before or after it, and an empty expansion there would erase
// the path they were about to build.
func expandTemplate(tmpl string, vars TemplateVars, keepHome bool) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	local, domain := splitUser(vars.User)
	var b strings.Builder
	b.Grow(len(tmpl))
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '%' {
			b.WriteByte(tmpl[i])
			continue
		}
		if i+1 >= len(tmpl) {
			return "", fmt.Errorf("path template %q: trailing %%", tmpl)
		}
		switch c := tmpl[i+1]; {
		case c == '%':
			b.WriteByte('%')
			i++
		case c == '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("path template %q: %%{ is never closed", tmpl)
			}
			if keepHome && strings.TrimSpace(tmpl[i+2:i+end]) == "home" {
				b.WriteString(tmpl[i : i+end+1])
				i += end
				continue
			}
			out, err := evalExpr(tmpl[i+2:i+end], vars, local, domain)
			if err != nil {
				return "", fmt.Errorf("path template %q: %w", tmpl, err)
			}
			b.WriteString(out)
			i += end
		case c >= '1' && c <= '9':
			out, adv, ok := expandHashVar(tmpl[i+1:], vars.User, local, domain)
			if !ok {
				return "", fmt.Errorf("path template %q: %%%s is not a hash variable (want %%<width>.<modulo>N<u|n|d>)", tmpl, tmpl[i+1:])
			}
			b.WriteString(out)
			i += adv
		case c == 'u':
			b.WriteString(vars.User)
			i++
		case c == 'n':
			b.WriteString(local)
			i++
		case c == 'd':
			b.WriteString(domain)
			i++
		case c == 'h':
			if keepHome {
				b.WriteString("%h")
			} else {
				b.WriteString(vars.Home)
			}
			i++
		default:
			return "", fmt.Errorf("path template %q: unknown variable %%%c", tmpl, c)
		}
	}
	return b.String(), nil
}

// ValidateTemplate reports whether tmpl can be expanded at all. Used at
// startup, where the point is to fail on a template nobody can satisfy rather
// than to produce a value: it expands against a stand-in user, so a template
// that only breaks on some particular username is not what this catches.
func ValidateTemplate(tmpl string) error {
	_, err := ExpandTemplate(tmpl, TemplateVars{User: "probe@example.org", Home: "/home/probe"})
	return err
}

// evalExpr evaluates the inside of a %{...}: a variable followed by filters,
// separated by "|".
func evalExpr(expr string, vars TemplateVars, local, domain string) (string, error) {
	parts := strings.Split(expr, "|")
	name := strings.TrimSpace(parts[0])

	// A value is either text or a hash still in its byte form. Keeping them
	// apart is what lets "sha1 % 256" mean the number and "sha1 | hex" the
	// digest, instead of one meaning the other by accident.
	var text string
	var digest []byte
	var number uint64
	haveNumber := false

	switch name {
	case "user":
		text = vars.User
	case "home":
		text = vars.Home
	default:
		return "", fmt.Errorf("unknown variable %q", name)
	}

	for _, raw := range parts[1:] {
		f := strings.TrimSpace(raw)
		switch {
		case f == "username":
			if digest != nil || haveNumber {
				return "", fmt.Errorf("filter %q needs text, not a hash", f)
			}
			text, _ = splitUser(text)
		case f == "domain":
			if digest != nil || haveNumber {
				return "", fmt.Errorf("filter %q needs text, not a hash", f)
			}
			_, text = splitUser(text)
		case f == "lower":
			if digest != nil || haveNumber {
				return "", fmt.Errorf("filter %q needs text, not a hash", f)
			}
			text = strings.ToLower(text)
		case f == "upper":
			if digest != nil || haveNumber {
				return "", fmt.Errorf("filter %q needs text, not a hash", f)
			}
			text = strings.ToUpper(text)
		case f == "sha1", strings.HasPrefix(f, "sha1 "), strings.HasPrefix(f, "sha1%"):
			sum := sha1.Sum([]byte(text))
			digest = sum[:]
			rest := strings.TrimSpace(strings.TrimPrefix(f, "sha1"))
			var err error
			if digest, number, haveNumber, err = applyModulo(digest, rest); err != nil {
				return "", err
			}
		case f == "md5", strings.HasPrefix(f, "md5 "), strings.HasPrefix(f, "md5%"):
			sum := md5.Sum([]byte(text))
			digest = sum[:]
			rest := strings.TrimSpace(strings.TrimPrefix(f, "md5"))
			var err error
			if digest, number, haveNumber, err = applyModulo(digest, rest); err != nil {
				return "", err
			}
		case strings.HasPrefix(f, "hex"):
			width, err := parseWidth(strings.TrimSpace(strings.TrimPrefix(f, "hex")))
			if err != nil {
				return "", err
			}
			if digest != nil {
				return "", fmt.Errorf("filter %q needs a number; a hash becomes one through a modulo (sha1 %% 256)", f)
			}
			if !haveNumber {
				n, perr := strconv.ParseUint(text, 10, 64)
				if perr != nil {
					return "", fmt.Errorf("filter %q needs a number, got %q", f, text)
				}
				number = n
			}
			text = padHex(number, width)
			haveNumber = false
		default:
			return "", fmt.Errorf("unknown filter %q", f)
		}
	}
	if digest != nil {
		return "", fmt.Errorf("expression %q ends in a hash; add a hex filter to make it a path segment", strings.TrimSpace(expr))
	}
	if haveNumber {
		return strconv.FormatUint(number, 10), nil
	}
	return text, nil
}

// applyModulo handles the "% <n>" tail of a hash filter. The digest is read as
// a big-endian unsigned integer from its **last** eight bytes, which is what the
// reference does; taking the first four instead would put every user in a
// different bucket than the config they migrated from.
func applyModulo(digest []byte, rest string) ([]byte, uint64, bool, error) {
	if rest == "" {
		return digest, 0, false, nil
	}
	if !strings.HasPrefix(rest, "%") {
		return nil, 0, false, fmt.Errorf("unexpected %q after a hash filter", rest)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(rest[1:]), 10, 32)
	if err != nil || n == 0 {
		return nil, 0, false, fmt.Errorf("modulo %q is not a positive number", strings.TrimSpace(rest[1:]))
	}
	var acc uint64
	for _, b := range digest[max(0, len(digest)-8):] {
		acc = acc<<8 | uint64(b)
	}
	return nil, acc % n, true, nil
}

// padHex renders n as hex, padded and truncated the way the reference does: a
// positive width left-pads with zeros and keeps the last width characters, a
// negative one right-pads and keeps the first.
func padHex(n uint64, width int) string {
	s := fmt.Sprintf("%x", n)
	switch {
	case width > 0:
		for len(s) < width {
			s = "0" + s
		}
		return s[len(s)-width:]
	case width < 0:
		w := -width
		for len(s) < w {
			s += "0"
		}
		return s[:w]
	}
	return s
}

// parseWidth reads the "(n)" of a hex filter. No argument means no padding.
func parseWidth(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return 0, fmt.Errorf("hex filter argument %q is not (n)", s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[1 : len(s)-1]))
	if err != nil || n == 0 {
		return 0, fmt.Errorf("hex width %q is not a non-zero number", strings.TrimSpace(s[1:len(s)-1]))
	}
	return n, nil
}
