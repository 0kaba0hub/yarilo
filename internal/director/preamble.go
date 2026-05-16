package director

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// preamble holds what the director learned from the protocol pre-auth exchange.
type preamble struct {
	username string // routing key for consistent-hash lookup
	cmdTag   string // IMAP command tag; empty for POP3/LMTP
	authLine string // bytes to replay to backend after connecting
}

// extractPreamble dispatches to the protocol-specific extractor.
func extractPreamble(conn net.Conn, rd *bufio.Reader, protocol string) (*preamble, error) {
	switch protocol {
	case "lmtp":
		return extractLMTPPreamble(conn, rd)
	default:
		return nil, fmt.Errorf("preamble: unsupported protocol %q (use login pods for imap/pop3/submission)", protocol)
	}
}

// extractLMTPPreamble speaks minimal LMTP until RCPT TO is received.
// The first recipient address is used as the routing key.
func extractLMTPPreamble(conn net.Conn, rd *bufio.Reader) (*preamble, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("lmtp: read LHLO: %w", err)
	}
	lhlo := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(strings.ToUpper(lhlo), "LHLO") {
		fmt.Fprintf(conn, "500 Expected LHLO\r\n")
		return nil, fmt.Errorf("lmtp: expected LHLO, got %q", lhlo)
	}
	fmt.Fprintf(conn, "250-yarilo\r\n250 8BITMIME\r\n")

	var buf strings.Builder
	buf.WriteString(lhlo + "\r\n")

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("lmtp: read: %w", err)
		}
		buf.WriteString(line)
		clean := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(clean)

		switch {
		case strings.HasPrefix(upper, "MAIL FROM:"):
			fmt.Fprintf(conn, "250 OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := extractAngleAddr(clean[8:])
			if addr == "" {
				fmt.Fprintf(conn, "550 Malformed address\r\n")
				continue
			}
			return &preamble{
				username: addr,
				authLine: buf.String(),
			}, nil
		case upper == "QUIT":
			fmt.Fprintf(conn, "221 Bye\r\n")
			return nil, fmt.Errorf("lmtp: client quit before RCPT TO")
		case upper == "NOOP":
			fmt.Fprintf(conn, "250 OK\r\n")
		default:
			fmt.Fprintf(conn, "500 Unknown command\r\n")
		}
	}
}

func extractAngleAddr(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") {
		return ""
	}
	return s[1 : len(s)-1]
}
