// Package loginproto defines the internal login-pod → backend preamble protocol.
// A login pod dials the backend and immediately sends one line before any IMAP/
// POP3/SMTP exchange starts. The backend reads the line, calls yarilo-auth VERIFY,
// and enters pre-authenticated state.
//
// Wire format: YARILO\tADDR=<ip>\tSESSION=<id>\tUSER=<user>\tTOKEN=<tok>[\tHELO=<domain>]\n
// TAB-delimited, LF-terminated, no CRLF, no xtext encoding (no special characters).
package loginproto

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// ErrNotYarilo is returned when the first line of a connection is not a YARILO preamble.
var ErrNotYarilo = errors.New("loginproto: not a YARILO preamble")

// Preamble carries the connection metadata sent by a login pod to a backend.
type Preamble struct {
	Addr      string // original client IP
	SessionID string // anvil session identifier
	User      string // authenticated username (verified by yarilo-auth)
	Token     string // one-time session token for yarilo-auth VERIFY
	Helo      string // EHLO/HELO domain (SMTP submission only)
}

// Format returns the wire-format preamble line (LF-terminated).
func (p Preamble) Format() string {
	var b strings.Builder
	b.WriteString("YARILO")
	if p.Addr != "" {
		b.WriteString("\tADDR=")
		b.WriteString(p.Addr)
	}
	if p.SessionID != "" {
		b.WriteString("\tSESSION=")
		b.WriteString(p.SessionID)
	}
	if p.User != "" {
		b.WriteString("\tUSER=")
		b.WriteString(p.User)
	}
	if p.Token != "" {
		b.WriteString("\tTOKEN=")
		b.WriteString(p.Token)
	}
	if p.Helo != "" {
		b.WriteString("\tHELO=")
		b.WriteString(p.Helo)
	}
	b.WriteByte('\n')
	return b.String()
}

// Parse reads and parses one preamble line from rd.
// Returns ErrNotYarilo if the line does not start with "YARILO".
func Parse(rd *bufio.Reader) (Preamble, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return Preamble{}, fmt.Errorf("loginproto: read: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	return ParseLine(line)
}

// ParseLine parses a raw preamble string (without the trailing newline).
func ParseLine(line string) (Preamble, error) {
	fields := strings.Split(line, "\t")
	if len(fields) < 1 || fields[0] != "YARILO" {
		return Preamble{}, ErrNotYarilo
	}
	var p Preamble
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "ADDR":
			p.Addr = v
		case "SESSION":
			p.SessionID = v
		case "USER":
			p.User = v
		case "TOKEN":
			p.Token = v
		case "HELO":
			p.Helo = v
		}
	}
	return p, nil
}
