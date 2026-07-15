package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

// AssignField populates a single field on UserInfo from its
// canonical name + string value. Used by every layer that
// reconstructs a UserInfo from a string-keyed source: the SQL
// userdb driver (SELECT row column names) and pkg/authclient
// (`key=value` wire pairs from the master-protocol response).
//
// Unknown keys land in UserInfo.Extra so a richer source schema
// does not lose data; `forward_*` keys auto-populate
// UserInfo.Forward. Boolean fields accept the canonical truthy
// values (1 / yes / y / true / t / on, case-insensitive); list
// fields (Groups, QuotaRules, AllowNets) accept comma-separated
// values and append, so a caller may invoke AssignField multiple
// times with the same key to merge entries (`quota_rule=` may
// appear repeatedly).
//
//nolint:gocyclo // long but each branch is one line of assignment
func AssignField(info *UserInfo, key, value string) error {
	switch strings.ToLower(key) {
	case "username", "user":
		info.Username = value
	case "original_user":
		info.OriginalUser = value
	case "master_user":
		info.MasterUser = value
	case "login_user":
		info.LoginUser = value
	case "uid":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return err
		}
		info.UID = uint32(n)
	case "gid":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return err
		}
		info.GID = uint32(n)
	case "home":
		info.Home = value
	case "chroot":
		info.Chroot = value
	case "system_groups_user":
		info.SystemGroupsUser = value
	case "groups":
		info.Groups = append(info.Groups, SplitCSV(value)...)
	case "acl_user":
		info.ACLUser = value
	case "acl_groups":
		info.ACLGroups = append(info.ACLGroups, SplitCSV(value)...)
	case "client_cert_present":
		info.ClientCertPresent = IsTruthy(value)
	case "volatile_dir":
		info.VolatileDir = value
	case "index_dir":
		info.IndexDir = value
	case "control_dir":
		info.ControlDir = value
	case "alt_dir":
		info.AltDir = value
	case "mail_path":
		info.MailPath = value
	case "mail_inbox_path":
		info.InboxPath = value
	case "mail", "mail_location":
		info.MailLocation = value
		// Extract the base path (driver:PATH[:modifiers]) as MailPath when
		// mail_path= was not set explicitly as a standalone userdb field.
		if info.MailPath == "" {
			if parts := strings.SplitN(value, ":", 3); len(parts) >= 2 && parts[1] != "" {
				info.MailPath = parts[1]
			}
		}
		// Explicit userdb fields (volatile_dir=, index_dir=, control_dir=)
		// take priority over modifiers embedded in the mail location string.
		// Only apply a modifier when the explicit field has not been set.
		if info.VolatileDir == "" {
			if vd := parseMailLocationMod(value, "VOLATILEDIR"); vd != "" {
				info.VolatileDir = vd
			}
		}
		if info.IndexDir == "" {
			if id := parseMailLocationMod(value, "INDEX"); id != "" {
				info.IndexDir = id
			}
		}
		if info.ControlDir == "" {
			if cd := parseMailLocationMod(value, "CONTROL"); cd != "" {
				info.ControlDir = cd
			}
		}
		if info.AltDir == "" {
			if ad := parseMailLocationMod(value, "ALT"); ad != "" {
				info.AltDir = ad
			}
		}
	case "mail_uid":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return err
		}
		info.MailUID = uint32(n)
	case "mail_gid":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return err
		}
		info.MailGID = uint32(n)
	case "mailbox_format":
		info.MailboxFormat = value
	case "mail_attribute_dict":
		info.MailAttributeDict = value
	case "quota_rule":
		info.QuotaRules = append(info.QuotaRules, SplitCSV(value)...)
	case "quota_over_flag":
		info.QuotaOverFlag = value
	case "allow_nets":
		info.AllowNets = append(info.AllowNets, SplitCSV(value)...)
	case "nologin":
		info.NoLogin = IsTruthy(value)
	case "nodelay":
		info.NoDelay = IsTruthy(value)
	case "noauthenticate":
		info.NoAuthenticate = IsTruthy(value)
	case "pass_expired":
		info.PassExpired = IsTruthy(value)
	case "nopassword":
		info.NoPassword = IsTruthy(value)
	case "proxy":
		info.Proxy = IsTruthy(value)
	case "proxy_maybe":
		info.ProxyMaybe = IsTruthy(value)
	case "host":
		info.Host = value
	case "port":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		info.Port = n
	case "destuser":
		info.DestUser = value
	case "proxy_mech":
		info.ProxyMech = value
	case "proxy_timeout":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		info.ProxyTimeout = n
	case "proxy_redirect_reauth":
		info.ProxyRedirectReauth = IsTruthy(value)
	case "proxy_nopipelining":
		info.ProxyNoPipelining = IsTruthy(value)
	case "ssl":
		info.SSL = value
	case "starttls":
		info.StartTLS = IsTruthy(value)
	case "mail_max_userip_connections":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		info.MailMaxUserIPConnections = n
	case "mail_max_user_connections":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		info.MailMaxUserConnections = n
	case "service":
		info.Service = value
	case "local_name":
		info.LocalName = value
	case "password":
		// `password` round-trips on the internal pipeline (passdb →
		// userdb prefetch in Phase AUTH-2) but the master-protocol
		// wire serialiser strips it before the bytes leave the
		// process. Keep the assignment so internal callers can
		// populate it.
		info.Password = value
	case "enabled":
		// Filter column from the default SQL userdb query — the
		// semantic is `WHERE enabled = 1` clause, not a UserInfo
		// field. Drop silently.
	default:
		if strings.HasPrefix(strings.ToLower(key), "forward_") {
			if info.Forward == nil {
				info.Forward = map[string]string{}
			}
			info.Forward[strings.TrimPrefix(strings.ToLower(key), "forward_")] = value
			return nil
		}
		if info.Extra == nil {
			info.Extra = map[string]string{}
		}
		info.Extra[key] = value
	}
	return nil
}

// SplitCSV splits a comma-separated value, trims whitespace around
// each entry, and drops empty entries. Returns nil for an empty or
// whitespace-only input so callers can assign the result directly
// to a `[]string` field without churning a slice the consumer must
// then filter.
func SplitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsTruthy reports whether s is one of the canonical truthy values
// auth-fields columns / wire pairs use (`1`, `yes`, `y`, `true`,
// `t`, `on`, case-insensitive). Anything else returns false —
// callers do not need to special-case empty strings or unknown
// values.
func IsTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "yes", "y", "true", "t", "on":
		return true
	}
	return false
}

// ParseFieldPair splits one `key=value` token from the
// master-protocol wire into (key, value), undoing the escapes
// marshalUserInfo applies for TAB / LF / NUL / backslash.
// Tokens without `=` are treated as bare flags with empty value
// (`nodelay` on its own is equivalent to `nodelay=yes`).
func ParseFieldPair(token string) (key, value string) {
	eq := strings.IndexByte(token, '=')
	if eq < 0 {
		return token, "yes"
	}
	return token[:eq], unescapeValue(token[eq+1:])
}

// unescapeValue reverses escapeValue. Stops at the first invalid
// escape and returns what was parsed so far — defensive, in case
// the peer ships malformed bytes.
func unescapeValue(v string) string {
	if !strings.ContainsRune(v, '\\') {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		default:
			// Unknown escape — preserve verbatim so the consumer
			// sees something rather than silently dropping data.
			b.WriteByte('\\')
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// ParseUserInfo reconstructs a UserInfo from a list of wire-format
// `key=value` tokens (typically the tab-split portion of a
// master-protocol USER / PASS response after the leading
// `<verb>\t<id>\t<username>\t...`). The Username is set by the
// caller from the wire's username token; ParseUserInfo only fills
// the remainder.
func ParseUserInfo(tokens []string) (*UserInfo, error) {
	info := &UserInfo{}
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		key, value := ParseFieldPair(tok)
		if err := AssignField(info, key, value); err != nil {
			return nil, fmt.Errorf("auth/protocol: parse field %q: %w", key, err)
		}
	}
	return info, nil
}

// parseMailLocationMod extracts the value of a named modifier from a
// mail location string of the form "driver:path:KEY1=v1:KEY2=v2".
// The lookup is case-insensitive. Returns "" when the modifier is absent.
func parseMailLocationMod(loc, mod string) string {
	parts := strings.Split(loc, ":")
	mod = strings.ToUpper(mod)
	for _, p := range parts[2:] {
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			if strings.ToUpper(p[:eq]) == mod {
				return p[eq+1:]
			}
		}
	}
	return ""
}
