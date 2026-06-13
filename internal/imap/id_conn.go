package imap

import (
	"bufio"
	"net"
	"runtime"
	"strings"
)

// idImapListener wraps a net.Listener so every accepted connection handles
// the IMAP ID extension (RFC 2971) at the connection level.
// go-imap/v2 has no server-side ID support, so we intercept the command here.
type idImapListener struct {
	net.Listener
	serverResp []byte // precomputed "* ID (...)\r\n"
}

func newIDListener(ln net.Listener, idSend string) net.Listener {
	pairs := parseIDSend(idSend)
	if len(pairs) == 0 {
		return ln
	}
	return &idImapListener{Listener: ln, serverResp: buildIDResponse(pairs)}
}

func (l *idImapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &idImapConn{Conn: c, br: bufio.NewReader(c), serverResp: l.serverResp}, nil
}

// idImapConn handles the IMAP ID extension at the connection level.
//
// Read side: "tag ID ..." commands are intercepted, answered with the
// server's ID data and "tag OK ID completed", and hidden from go-imap/v2.
//
// Write side: "ID" is appended to every "* CAPABILITY ..." line so clients
// know they may send the ID command.
type idImapConn struct {
	net.Conn
	br         *bufio.Reader
	pending    []byte
	serverResp []byte
}

func (c *idImapConn) Unwrap() net.Conn { return c.Conn }

func (c *idImapConn) Read(b []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			n := copy(b, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}
		line, err := c.br.ReadString('\n')
		if len(line) > 0 {
			fields := strings.Fields(strings.TrimRight(line, "\r\n"))
			if len(fields) >= 2 && strings.ToUpper(fields[1]) == "ID" {
				tag := fields[0]
				resp := append(append([]byte(nil), c.serverResp...), []byte(tag+" OK ID completed\r\n")...)
				c.Conn.Write(resp) //nolint:errcheck
				if err != nil {
					return 0, err
				}
				continue
			}
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

func (c *idImapConn) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	if strings.HasPrefix(line, "* CAPABILITY ") {
		hasID := strings.Contains(line, " ID ") || strings.HasSuffix(line, " ID")
		if !hasID {
			return c.Conn.Write([]byte(line + " ID\r\n"))
		}
	}
	return c.Conn.Write(b)
}

// buildIDResponse builds the "* ID (...)\r\n" response from a flat [key, val, ...] slice.
func buildIDResponse(pairs []string) []byte {
	var sb strings.Builder
	sb.WriteString("* ID (")
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteByte('"')
		sb.WriteString(pairs[i])
		sb.WriteString(`" "`)
		sb.WriteString(pairs[i+1])
		sb.WriteByte('"')
	}
	sb.WriteString(")\r\n")
	return []byte(sb.String())
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
