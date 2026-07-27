package submission

import (
	"bufio"
	"net"
	"strings"
)

type submissionWorkarounds uint32

const (
	workaroundWhitespaceBeforePath submissionWorkarounds = 1 << iota
	workaroundMailboxForPath
)

func parseWorkarounds(list []string) submissionWorkarounds {
	var mask submissionWorkarounds
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "whitespace-before-path":
			mask |= workaroundWhitespaceBeforePath
		case "mailbox-for-path":
			mask |= workaroundMailboxForPath
		}
	}
	return mask
}

type workaroundListener struct {
	net.Listener
	workarounds submissionWorkarounds
}

func (l *workaroundListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &workaroundConn{Conn: c, br: bufio.NewReader(c), workarounds: l.workarounds}, nil
}

type workaroundConn struct {
	net.Conn
	br          *bufio.Reader
	pending     []byte
	workarounds submissionWorkarounds
}

// Unwrap exposes the wrapped conn so the server can walk to the
// *loginproto.PreambleConn carrying pre-auth state (#830). Since #828 put this
// wrapper ABOVE the PreambleListener, the direct type-assertion stopped seeing
// the PreambleConn and every session started unauthenticated.
func (c *workaroundConn) Unwrap() net.Conn { return c.Conn }

func (c *workaroundConn) Read(b []byte) (int, error) {
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

func (c *workaroundConn) applyWorkarounds(line string) string {
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
