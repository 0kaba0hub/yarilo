package protocol

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Validator parses a raw field value into its canonical string form,
// or errors if the value violates the field's type contract.
// Equivalent inputs normalise to one representation ("TRUE"/"yes"/"1"
// all become "yes"), so two callers writing the same field produce
// byte-identical wire output.
type Validator func(value string) (string, error)

// reservedValidators is the registry of typed auth fields, keyed by
// BASE field name. SetValidated strips the userdb_ prefix before lookup
// so userdb_uid validates as uid. forward_* fields pass through
// unvalidated: they are opaque to the chain and must survive verbatim
// through the proxy hop.
var reservedValidators = map[string]Validator{
	// Numeric — uint32.
	"uid":      validateUint32,
	"gid":      validateUint32,
	"mail_uid": validateUint32,
	"mail_gid": validateUint32,

	// Numeric — int.
	"port":                        validateInt,
	"proxy_timeout":               validateInt,
	"mail_max_userip_connections": validateInt,
	"mail_max_user_connections":   validateInt,

	// Boolean flags. Canonical truthy = "yes", falsy = "no".
	"nologin":               validateBool,
	"nodelay":               validateBool,
	"noauthenticate":        validateBool,
	"pass_expired":          validateBool,
	"nopassword":            validateBool,
	"proxy":                 validateBool,
	"proxy_maybe":           validateBool,
	"proxy_redirect_reauth": validateBool,
	"proxy_nopipelining":    validateBool,
	"starttls":              validateBool,
	"client_cert_present":   validateBool,

	// Lists — non-empty, comma-separated. Each entry validated.
	"allow_nets": validateCIDRList,
	"groups":     validateNonEmptyCSV,
	"quota_rule": validateNonEmptyCSV,

	// Enum.
	"ssl": validateSSLEnum,
}

// validateUint32 accepts a decimal uint32; returns the canonical
// decimal form (strips leading zeros, rejects negatives / >2^32).
func validateUint32(v string) (string, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
	if err != nil {
		return "", fmt.Errorf("not a uint32: %w", err)
	}
	return strconv.FormatUint(n, 10), nil
}

// validateInt accepts a decimal int (machine-width); returns the
// canonical decimal form.
func validateInt(v string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("not an int: %w", err)
	}
	return strconv.Itoa(n), nil
}

// validateBool accepts the truthy/falsy literals passdb columns and
// admin tooling produce. Canonical output: "yes"/"no", matching what
// VisitFields emits via the "yes" shortcut.
func validateBool(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "on", "1", "t", "y":
		return "yes", nil
	case "no", "false", "off", "0", "f", "n":
		return "no", nil
	}
	return "", fmt.Errorf("not a boolean: %q (accept yes/no/true/false/on/off/1/0)", v)
}

// validateCIDRList accepts a comma-separated list of IP/CIDR strings.
// Single IPs are promoted to /32 (IPv4) or /128 (IPv6) so allow_nets=10.0.0.5
// works without the suffix. Returns the canonical comma-joined CIDR form.
func validateCIDRList(v string) (string, error) {
	parts := splitTrim(v)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty CIDR list")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		_, ipnet, err := net.ParseCIDR(p)
		if err == nil {
			out = append(out, ipnet.String())
			continue
		}
		if ip := net.ParseIP(p); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, fmt.Sprintf("%s/%d", ip.String(), bits))
			continue
		}
		return "", fmt.Errorf("invalid CIDR or IP %q", p)
	}
	return strings.Join(out, ","), nil
}

// validateNonEmptyCSV accepts a comma-separated list where every
// entry must be non-empty after trimming whitespace. Returns the
// canonical comma-joined form (no surrounding whitespace).
func validateNonEmptyCSV(v string) (string, error) {
	parts := splitTrim(v)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty list")
	}
	return strings.Join(parts, ","), nil
}

// validateSSLEnum accepts the three SSL-mode tokens used by
// proxy / namespace settings: "yes" / "any" / "required".
// Case-insensitive on input; lowercase on output.
func validateSSLEnum(v string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(v))
	switch norm {
	case "yes", "any", "required":
		return norm, nil
	}
	return "", fmt.Errorf("invalid ssl mode %q (want yes/any/required)", v)
}

// splitTrim is the canonical CSV splitter for validators: splits
// on `,`, trims each entry, drops empties. Returns nil for input
// that yields no entries.
func splitTrim(v string) []string {
	raw := strings.Split(v, ",")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}
