package imap

import (
	"runtime"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
)

// ID answers the IMAP ID command (RFC 2971) with what imap_id_send configures.
//
// This used to be a connection wrapper that scanned the byte stream for a line
// whose second token was "ID". A wrapper cannot tell a command from message
// data: a body line inside an APPEND literal was answered and removed from the
// stream, and the literal made up the missing octets from the command that
// followed (#1375). The parser knows where a literal is, so the distinction is
// now structural.
func (s *session) ID(_ *imaplib.IDData) *imaplib.IDData {
	pairs := parseIDSend(s.srv.opts.IDSend)
	if len(pairs) == 0 {
		return nil
	}
	data := &imaplib.IDData{Raw: make(map[string]string, len(pairs)/2)}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, val := strings.ToLower(pairs[i]), pairs[i+1]
		data.Raw[key] = val
		switch key {
		case "name":
			data.Name = val
		case "version":
			data.Version = val
		case "os":
			data.OS = val
		case "os-version":
			data.OSVersion = val
		case "vendor":
			data.Vendor = val
		case "support-url":
			data.SupportURL = val
		case "address":
			data.Address = val
		case "date":
			data.Date = val
		case "command":
			data.Command = val
		case "arguments":
			data.Arguments = val
		case "environment":
			data.Environment = val
		}
	}
	return data
}

// parseIDSend parses the imap_id_send config string ("key value key value ...")
// and resolves "*" to server defaults.
func parseIDSend(s string) []string {
	fields := strings.Fields(s)
	var pairs []string
	for i := 0; i+1 < len(fields); i += 2 {
		k := fields[i]
		v := resolveIDValue(k, fields[i+1])
		if v != "" {
			pairs = append(pairs, k, v)
		}
	}
	return pairs
}

// resolveIDValue expands "*" to the server default for known keys.
func resolveIDValue(key, val string) string {
	if val != "*" {
		return val
	}
	switch key {
	case "name":
		return "yarilo"
	case "version":
		return "dev"
	case "os":
		return runtime.GOOS
	case "os-version":
		return runtime.Version()
	default:
		return ""
	}
}
