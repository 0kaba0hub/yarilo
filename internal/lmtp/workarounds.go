package lmtp

import (
	"bufio"
	"net"
	"strings"
)

// lmtpWorkarounds is a bitmask of active client workarounds.
type lmtpWorkarounds uint32

const (
	// workaroundWhitespaceBeforePath allows whitespace before the path in
	// MAIL FROM and RCPT TO commands (e.g. "MAIL FROM: <user@example.com>").
	workaroundWhitespaceBeforePath lmtpWorkarounds = 1 << iota
	// workaroundMailboxForPath allows a bare mailbox name (no domain) in
	// RCPT TO (e.g. "RCPT TO:<alice>").
	workaroundMailboxForPath
)

// parseWorkarounds parses a list of workaround names into a bitmask.
// Names it does not recognise are returned rather than dropped: an accepted
// setting that does nothing is the hardest kind of configuration bug to see.
func parseWorkarounds(list []string) (lmtpWorkarounds, []string) {
	var mask lmtpWorkarounds
	var unknown []string
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "whitespace-before-path":
			mask |= workaroundWhitespaceBeforePath
		case "mailbox-for-path":
			mask |= workaroundMailboxForPath
		case "":
		default:
			unknown = append(unknown, item)
		}
	}
	return mask, unknown
}

type lmtpWorkaroundListener struct {
	net.Listener
	workarounds lmtpWorkarounds
}

func (l *lmtpWorkaroundListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &lmtpWorkaroundConn{Conn: c, br: bufio.NewReader(c), workarounds: l.workarounds}, nil
}

type lmtpWorkaroundConn struct {
	net.Conn
	br          *bufio.Reader
	pending     []byte
	workarounds lmtpWorkarounds
}

func (c *lmtpWorkaroundConn) Read(b []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			n := copy(b, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}
		line, err := c.br.ReadString('\n')
		if len(line) > 0 {
			line = c.applyWorkarounds(line)
			n := copy(b, []byte(line))
			if n < len(line) {
				c.pending = []byte(line)[n:]
			}
			return n, err
		}
		if err != nil {
			return 0, err
		}
	}
}

func (c *lmtpWorkaroundConn) applyWorkarounds(line string) string {
	upper := strings.ToUpper(line)
	var prefixLen int
	switch {
	case strings.HasPrefix(upper, "MAIL FROM:"):
		prefixLen = len("MAIL FROM:")
	case strings.HasPrefix(upper, "RCPT TO:"):
		prefixLen = len("RCPT TO:")
	default:
		return line
	}
	prefix := line[:prefixLen]
	rest := line[prefixLen:]

	if c.workarounds&workaroundWhitespaceBeforePath != 0 {
		rest = strings.TrimLeft(rest, " \t")
	}
	if c.workarounds&workaroundMailboxForPath != 0 {
		trimmed := strings.TrimRight(rest, "\r\n")
		if trimmed != "" && !strings.HasPrefix(trimmed, "<") {
			suffix := rest[len(trimmed):]
			rest = "<" + trimmed + ">" + suffix
		}
	}
	return prefix + rest
}

// knownWorkarounds is the accepted set, so the warning about an unknown name
// can print what the operator could have meant.
func knownWorkarounds() []string {
	return []string{"whitespace-before-path", "mailbox-for-path"}
}
